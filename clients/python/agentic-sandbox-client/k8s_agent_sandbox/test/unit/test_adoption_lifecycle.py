# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Public client lifecycle regressions at the Kubernetes API boundary."""

from copy import deepcopy
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch
import asyncio
import inspect

import pytest
import pytest_asyncio
from kubernetes.client import ApiException
from kubernetes_asyncio.client import ApiException as AsyncApiException
from urllib3.exceptions import ReadTimeoutError

from k8s_agent_sandbox import constants as C
from k8s_agent_sandbox.async_sandbox_client import AsyncSandboxClient
from k8s_agent_sandbox.sandbox_client import SandboxClient
from k8s_agent_sandbox.models import SandboxDirectConnectionConfig
from k8s_agent_sandbox.exceptions import SandboxNotFoundError
from k8s_agent_sandbox.gke_extensions.snapshots.podsnapshot_client import PodSnapshotSandboxClient


def merge_patch(target, patch_body):
    for key, value in patch_body.items():
        if value is None:
            target.pop(key, None)
        elif isinstance(value, dict):
            merge_patch(target.setdefault(key, {}), value)
        else:
            target[key] = deepcopy(value)


class MemoryApi:
    def __init__(self):
        self.objects = {}
        self.deletes = []
        self.fail_after_claim_persist = None
        self.claim_ready = True

    def create_namespaced_custom_object(self, *, namespace, plural, body, **_):
        name = body['metadata']['name']
        key = (plural, namespace, name)
        if key in self.objects:
            raise ApiException(status=409)
        obj = deepcopy(body)
        obj['metadata'].update(namespace=namespace, uid=f'uid-{name}', resourceVersion='10', generation=1)
        if plural == C.CLAIM_PLURAL_NAME:
            obj['status'] = {'sandbox': {'name': 'sandbox-a'}, 'conditions': [
                {'type': 'Ready', 'status': 'True' if self.claim_ready else 'False', 'observedGeneration': 1}
            ]}
            self.objects[(C.SANDBOX_PLURAL_NAME, namespace, 'sandbox-a')] = {
                'metadata': {'name': 'sandbox-a'},
                'spec': {'operatingMode': 'Running', 'podTemplate': {'metadata': {}}},
                'status': {'selector': f'{C.SANDBOX_NAME_HASH_LABEL}=hash-a', 'podIPs': ['10.0.0.2']},
            }
        if plural == C.PODSNAPSHOTMANUALTRIGGER_PLURAL:
            obj['status'] = {'snapshotCreated': {'name': 'snapshot-a'}, 'conditions': [{
                'type': 'Triggered', 'status': 'True', 'reason': 'Complete',
                'lastTransitionTime': '2026-09-23T16:00:00Z',
            }]}
            self.objects[(C.PODSNAPSHOT_PLURAL, namespace, 'snapshot-a')] = {
                'metadata': {'name': 'snapshot-a'},
                'status': {'conditions': [{'type': 'Ready', 'status': 'True'}]},
            }
        self.objects[key] = obj
        if plural == C.CLAIM_PLURAL_NAME and self.fail_after_claim_persist is not None:
            raise self.fail_after_claim_persist
        return deepcopy(obj)

    def get_namespaced_custom_object(self, *, namespace, plural, name, **_):
        key = (plural, namespace, name)
        if key not in self.objects:
            raise ApiException(status=404)
        return deepcopy(self.objects[key])

    def delete_namespaced_custom_object(self, *, namespace, plural, name, body=None, **_):
        key = (plural, namespace, name)
        if body and key in self.objects and body.preconditions.uid != self.objects[key]['metadata']['uid']:
            raise ApiException(status=409)
        self.deletes.append(key)
        self.objects.pop(key, None)

    def patch_namespaced_custom_object(self, *, namespace, plural, name, body, **_):
        obj = self.objects[(plural, namespace, name)]
        merge_patch(obj, body)
        if plural == C.CLAIM_PLURAL_NAME:
            obj['metadata']['generation'] += 1
            obj['metadata']['resourceVersion'] = '11'
            obj['status']['conditions'][0]['observedGeneration'] = obj['metadata']['generation']
            annotations = obj['spec'].get('additionalPodMetadata', {}).get('annotations', {})
            sandbox = self.objects[(C.SANDBOX_PLURAL_NAME, namespace, 'sandbox-a')]
            sandbox['spec']['podTemplate']['metadata']['annotations'] = deepcopy(annotations)
        return deepcopy(obj)

    def list_namespaced_custom_object(self, *, namespace, plural, field_selector=None, **_):
        return {'items': [deepcopy(obj) for (p, ns, name), obj in self.objects.items()
                          if p == plural and ns == namespace and
                          (not field_selector or field_selector == f'metadata.name={name}') ]}

    def get_api_resources(self, **_):
        return SimpleNamespace(resources=[SimpleNamespace(kind=C.PODSNAPSHOT_API_KIND)])

    def remaining(self, plural):
        return [key for key in self.objects if key[0] == plural]


@pytest.fixture
def environment():
    api = MemoryApi()
    core_api = SimpleNamespace(read_namespaced_pod=lambda *_: SimpleNamespace(
        metadata=SimpleNamespace(deletion_timestamp=None),
        status=SimpleNamespace(conditions=[
            SimpleNamespace(type='Ready', status='True'),
            SimpleNamespace(type='PodRestored', status='True', message='snapshot-a restored'),
        ]),
    ))

    class Watch:
        def stream(self, func, **kwargs):
            for obj in func(**kwargs)['items']:
                yield {'type': 'MODIFIED', 'object': obj}

        def stop(self):
            pass

    with patch('kubernetes.config.load_incluster_config'), \
            patch('kubernetes.client.CustomObjectsApi', return_value=api), \
            patch('kubernetes.client.CoreV1Api', return_value=core_api), \
            patch('kubernetes.watch.Watch', Watch), \
            patch('atexit.register') as atexit_register:
        yield api, core_api, atexit_register


@pytest.mark.parametrize('cleanup_path', ['delete_sandbox', 'delete_all', 'atexit'])
def test_snapshot_trigger_cleanup(environment, cleanup_path):
    api, _, register = environment
    client = PodSnapshotSandboxClient(cleanup=True)
    sandbox = client.create_sandbox('pool-a')
    snapshot = sandbox.snapshots.create('diagnostic')
    assert snapshot.success
    assert len(api.remaining(C.PODSNAPSHOTMANUALTRIGGER_PLURAL)) == 1
    if cleanup_path == 'delete_sandbox':
        client.delete_sandbox(sandbox.claim_name)
    elif cleanup_path == 'delete_all':
        client.delete_all()
    else:
        register.call_args.args[0]()
    assert api.remaining(C.CLAIM_PLURAL_NAME) == []
    assert api.remaining(C.PODSNAPSHOTMANUALTRIGGER_PLURAL) == [], 'manual trigger leaked'


def test_generated_claim_rollback_after_lost_response(environment):
    api, _, _ = environment
    api.fail_after_claim_persist = ReadTimeoutError(None, '/sandboxclaims', 'response lost after persistence')
    client = SandboxClient()
    with pytest.raises(ReadTimeoutError):
        client.create_sandbox('pool-a')
    assert api.remaining(C.CLAIM_PLURAL_NAME) == [], 'persisted generated claim leaked'


def test_async_generated_claim_rollback_after_lost_response():
    async def run():
        api = MemoryApi()
        api.fail_after_claim_persist = asyncio.TimeoutError('response lost after persistence')
        async_api = SimpleNamespace(
            create_namespaced_custom_object=AsyncMock(side_effect=api.create_namespaced_custom_object),
            delete_namespaced_custom_object=AsyncMock(side_effect=api.delete_namespaced_custom_object),
        )
        with patch('kubernetes_asyncio.config.load_incluster_config'), \
                patch('kubernetes_asyncio.client.CustomObjectsApi', return_value=async_api), \
                patch('kubernetes_asyncio.client.ApiClient', return_value=SimpleNamespace(close=AsyncMock())):
            client = AsyncSandboxClient(
                connection_config=SandboxDirectConnectionConfig(api_url='http://unused.invalid'),
                cleanup=False,
            )
            with pytest.raises(asyncio.TimeoutError):
                await client.create_sandbox('pool-a')
            await client.close()
        assert api.remaining(C.CLAIM_PLURAL_NAME) == [], 'persisted async generated claim leaked'
    asyncio.run(run())


def test_adoption_after_successful_snapshot_restore(environment):
    api, _, _ = environment
    client = PodSnapshotSandboxClient()
    sandbox = client.create_sandbox('pool-a', claim_name='workflow-a', adopt_existing=True)
    original_uid = api.objects[(C.CLAIM_PLURAL_NAME, 'default', 'workflow-a')]['metadata']['uid']
    assert sandbox.snapshots.create('diagnostic').success
    api.objects[(C.SANDBOX_PLURAL_NAME, 'default', 'sandbox-a')]['spec']['operatingMode'] = 'Suspended'
    api.objects[(C.SANDBOX_PLURAL_NAME, 'default', 'sandbox-a')]['status']['podIPs'] = []
    restored = sandbox.restore('snapshot-a', sandbox_ready_timeout=1)
    assert restored.success and restored.restored_from_snapshot
    claim = api.objects[(C.CLAIM_PLURAL_NAME, 'default', 'workflow-a')]
    assert claim['metadata']['uid'] == original_uid
    assert claim['spec']['additionalPodMetadata']['annotations'][C.PODSNAPSHOT_NAME_ANNOTATION] == 'snapshot-a'
    retry_client = PodSnapshotSandboxClient()
    retry = retry_client.create_sandbox('pool-a', claim_name='workflow-a', adopt_existing=True)
    assert retry.sandbox_id == sandbox.sandbox_id


def test_retry_on_same_client_preserves_snapshot_cleanup(environment):
    api, _, _ = environment
    client = PodSnapshotSandboxClient()
    sandbox = client.create_sandbox('pool-a', claim_name='workflow-a')
    assert sandbox.snapshots.create('diagnostic').success
    client.create_sandbox('pool-a', claim_name='workflow-a', adopt_existing=True)
    client.delete_all()
    assert api.remaining(C.PODSNAPSHOTMANUALTRIGGER_PLURAL) == []


async def invoke(method, *args, **kwargs):
    result = method(*args, **kwargs)
    return await result if inspect.isawaitable(result) else result


@pytest_asyncio.fixture(params=['sync', 'async'])
async def sdk_client(request, environment):
    api, core_api, register = environment

    def operation(name):
        async def call(**kwargs):
            try:
                return getattr(api, name)(**kwargs)
            except ApiException as error:
                raise AsyncApiException(status=error.status) from error
        return call

    class AsyncWatch:
        async def stream(self, func, **kwargs):
            for obj in (await func(**kwargs))['items']:
                yield {'type': 'MODIFIED', 'object': obj}

        async def close(self):
            pass

    async_api = SimpleNamespace(**{
        name: operation(name) for name in (
            'create_namespaced_custom_object', 'get_namespaced_custom_object',
            'delete_namespaced_custom_object', 'list_namespaced_custom_object',
        )
    })
    with patch('kubernetes_asyncio.config.load_incluster_config'), \
            patch('kubernetes_asyncio.client.CustomObjectsApi', return_value=async_api), \
            patch('kubernetes_asyncio.client.CoreV1Api', return_value=core_api), \
            patch('kubernetes_asyncio.client.ApiClient', return_value=SimpleNamespace(close=AsyncMock())), \
            patch('kubernetes_asyncio.watch.Watch', AsyncWatch):
        client = (SandboxClient(cleanup=True) if request.param == 'sync' else
                  AsyncSandboxClient(SandboxDirectConnectionConfig(api_url='http://unused.invalid')))
        yield api, client, register
        if request.param == 'async':
            await client.close()


def seed_claim(api, name='workflow-a'):
    return api.create_namespaced_custom_object(
        namespace='default', plural=C.CLAIM_PLURAL_NAME,
        body={'apiVersion': f'{C.CLAIM_API_GROUP}/{C.CLAIM_API_VERSION}',
              'kind': 'SandboxClaim', 'metadata': {'name': name},
              'spec': {'warmPoolRef': {'name': 'pool-a'}}},
    )


@pytest.mark.asyncio
async def test_explicit_creation_and_cross_client_adoption(sdk_client):
    api, client, _ = sdk_client
    existing = seed_claim(api)
    with patch.object(api, 'list_namespaced_custom_object', side_effect=AssertionError('no future events')):
        sandbox = await invoke(client.create_sandbox, 'pool-a', claim_name='workflow-a', adopt_existing=True)
    assert sandbox.claim_name == 'workflow-a'
    assert sandbox.sandbox_id == 'sandbox-a'
    assert api.objects[(C.CLAIM_PLURAL_NAME, 'default', 'workflow-a')] == existing


@pytest.mark.asyncio
@pytest.mark.parametrize('name', ['', 'UPPERCASE', 'has_underscore', '-leading', 'a' * 254])
async def test_invalid_claim_name_never_creates_resources(sdk_client, name):
    api, client, _ = sdk_client
    with pytest.raises(ValueError, match='DNS-1123'):
        await invoke(client.create_sandbox, 'pool-a', claim_name=name)
    assert await invoke(client.list_all_sandboxes) == []


@pytest.mark.asyncio
@pytest.mark.parametrize('name', ['workflow.retry-1', 'a' * 64, 'a' * 253])
async def test_valid_dns_subdomain_names(sdk_client, name):
    _, client, _ = sdk_client
    sandbox = await invoke(client.create_sandbox, 'pool-a', claim_name=name)
    assert sandbox.claim_name == name
    assert await invoke(client.list_all_sandboxes) == [name]


@pytest.mark.asyncio
async def test_adoption_requires_explicit_name(sdk_client):
    _, client, _ = sdk_client
    with pytest.raises(ValueError, match='explicit claim_name'):
        await invoke(client.create_sandbox, 'pool-a', adopt_existing=True)
    assert await invoke(client.list_all_sandboxes) == []


@pytest.mark.asyncio
@pytest.mark.parametrize('status', [409, 403, 500])
async def test_conflicts_require_opt_in_and_other_errors_propagate(sdk_client, status):
    api, client, _ = sdk_client
    with patch.object(api, 'create_namespaced_custom_object', side_effect=ApiException(status=status)):
        with pytest.raises((ApiException, AsyncApiException)) as raised:
            await invoke(client.create_sandbox, 'pool-a', claim_name='workflow-a', adopt_existing=status != 409)
    assert raised.value.status == status
    assert api.deletes == []


@pytest.mark.asyncio
@pytest.mark.parametrize('mismatch', ['pool', 'terminating'])
async def test_incompatible_claim_is_not_adopted_or_deleted(sdk_client, mismatch):
    api, client, _ = sdk_client
    seed_claim(api)
    claim = api.objects[(C.CLAIM_PLURAL_NAME, 'default', 'workflow-a')]
    if mismatch == 'pool':
        claim['spec']['warmPoolRef']['name'] = 'other-pool'
    else:
        claim['metadata']['deletionTimestamp'] = '2026-09-23T16:00:00Z'
    with pytest.raises(ValueError, match='warm pool|terminating'):
        await invoke(client.create_sandbox, 'pool-a', claim_name='workflow-a', adopt_existing=True)
    assert await invoke(client.list_all_sandboxes) == ['workflow-a']
    assert api.deletes == []


@pytest.mark.asyncio
async def test_disappearing_claim_after_conflict_is_retryable(sdk_client):
    api, client, _ = sdk_client
    with patch.object(api, 'create_namespaced_custom_object', side_effect=ApiException(status=409)):
        with pytest.raises(SandboxNotFoundError, match='retry'):
            await invoke(client.create_sandbox, 'pool-a', claim_name='workflow-a', adopt_existing=True)
    assert api.deletes == []


@pytest.mark.asyncio
async def test_adoption_keeps_original_shutdown_and_mutable_metadata(sdk_client):
    api, client, _ = sdk_client
    await invoke(client.create_sandbox, 'pool-a', claim_name='workflow-a', shutdown_after_seconds=300)
    key = (C.CLAIM_PLURAL_NAME, 'default', 'workflow-a')
    original_lifecycle = deepcopy(api.objects[key]['spec']['lifecycle'])
    api.objects[key]['spec']['additionalPodMetadata'] = {'annotations': {C.PODSNAPSHOT_NAME_ANNOTATION: 'snapshot-a'}}
    await invoke(client.create_sandbox, 'pool-a', claim_name='workflow-a', adopt_existing=True, shutdown_after_seconds=900)
    assert api.objects[key]['spec']['lifecycle'] == original_lifecycle
    assert api.objects[key]['spec']['additionalPodMetadata']['annotations'][C.PODSNAPSHOT_NAME_ANNOTATION] == 'snapshot-a'


@pytest.mark.asyncio
async def test_automatic_cleanup_preserves_explicit_claim_but_delete_all_is_deliberate(sdk_client):
    _, client, register = sdk_client
    explicit = await invoke(client.create_sandbox, 'pool-a', claim_name='workflow-a')
    generated = await invoke(client.create_sandbox, 'pool-a')
    generated_name = generated.claim_name
    register.call_args.args[0]()
    assert await invoke(client.list_all_sandboxes) == ['workflow-a']
    assert not explicit.is_active
    assert generated_name != 'workflow-a'
    await invoke(client.delete_all)
    assert await invoke(client.list_all_sandboxes) == []


@pytest.mark.asyncio
async def test_failed_readiness_preserves_explicit_claim(sdk_client):
    api, client, _ = sdk_client
    api.claim_ready = False
    with patch.object(api, 'list_namespaced_custom_object', side_effect=TimeoutError('readiness timeout')):
        with pytest.raises(TimeoutError):
            await invoke(client.create_sandbox, 'pool-a', claim_name='workflow-a')
    assert await invoke(client.list_all_sandboxes) == ['workflow-a']
    assert api.deletes == []


@pytest.mark.asyncio
async def test_uid_guard_survives_watch_compaction(sdk_client):
    api, client, _ = sdk_client
    api.claim_ready = False
    replacement = seed_claim(api)
    replacement['metadata']['uid'] = 'replacement-uid'
    replacement['status']['conditions'][0]['status'] = 'True'
    with patch.object(api, 'list_namespaced_custom_object', side_effect=[ApiException(status=410), {'items': [replacement]}]) as stream:
        with pytest.raises(SandboxNotFoundError, match='replaced'):
            await invoke(client.create_sandbox, 'pool-a', claim_name='workflow-a', adopt_existing=True)
    assert [call.kwargs['resource_version'] for call in stream.call_args_list] == ['10', '0']
    assert api.deletes == []


@pytest.mark.asyncio
async def test_adoption_resumes_same_claim_after_watch_compaction(sdk_client):
    api, client, _ = sdk_client
    api.claim_ready = False
    ready = seed_claim(api)
    ready['status']['conditions'][0]['status'] = 'True'
    with patch.object(api, 'list_namespaced_custom_object', side_effect=[
        ApiException(status=410), {'items': [ready]},
    ]) as stream:
        sandbox = await invoke(client.create_sandbox, 'pool-a', claim_name='workflow-a', adopt_existing=True)
    assert sandbox.sandbox_id == 'sandbox-a'
    assert [call.kwargs['resource_version'] for call in stream.call_args_list] == ['10', '0']
    assert api.deletes == []


@pytest.mark.asyncio
@pytest.mark.parametrize('sdk_client', ['async'], indirect=True)
async def test_async_context_cleanup_preserves_explicit_claim(sdk_client):
    api, client, _ = sdk_client
    async with client:
        explicit = await client.create_sandbox('pool-a', claim_name='workflow-a')
        generated = await client.create_sandbox('pool-a')
    assert api.remaining(C.CLAIM_PLURAL_NAME) == [(C.CLAIM_PLURAL_NAME, 'default', 'workflow-a')]
    assert not explicit.is_active
    assert not generated.is_active


@pytest.mark.asyncio
@pytest.mark.parametrize('sdk_client', ['async'], indirect=True)
async def test_async_close_can_retry_failed_connection_cleanup(sdk_client):
    _, client, _ = sdk_client
    sandbox = await client.create_sandbox('pool-a', claim_name='workflow-a')
    with patch('httpx.AsyncClient.aclose', side_effect=RuntimeError('connection close failed')):
        await client.close()
    assert await client.list_active_sandboxes() == [('default', 'workflow-a')]
    await client.close()
    assert not sandbox.is_active
    assert await client.list_active_sandboxes() == []
    assert await client.list_all_sandboxes() == ['workflow-a']


@pytest.mark.asyncio
@pytest.mark.parametrize('sdk_client', ['async'], indirect=True)
@pytest.mark.parametrize('claim_name', [None, 'workflow-a'])
async def test_async_cancelled_creation_cleans_only_generated_claim(sdk_client, claim_name):
    api, client, _ = sdk_client
    api.fail_after_claim_persist = asyncio.CancelledError()
    with pytest.raises(asyncio.CancelledError):
        await client.create_sandbox('pool-a', claim_name=claim_name)
    assert await client.list_all_sandboxes() == ([] if claim_name is None else ['workflow-a'])


@pytest.mark.asyncio
async def test_generated_conflict_does_not_delete_an_existing_claim(sdk_client):
    api, client, _ = sdk_client
    with patch.object(api, 'create_namespaced_custom_object', side_effect=ApiException(status=409)):
        with pytest.raises((ApiException, AsyncApiException)):
            await invoke(client.create_sandbox, 'pool-a')
    assert api.deletes == []


@pytest.mark.asyncio
async def test_generated_rollback_with_known_uid_does_not_delete_a_replacement(sdk_client):
    api, client, _ = sdk_client
    api.claim_ready = False

    def replace_then_timeout(**_):
        key = api.remaining(C.CLAIM_PLURAL_NAME)[0]
        api.objects[key]['metadata']['uid'] = 'replacement-uid'
        raise TimeoutError('readiness timeout')

    with patch.object(api, 'list_namespaced_custom_object', side_effect=replace_then_timeout):
        with pytest.raises(TimeoutError, match='readiness timeout'):
            await invoke(client.create_sandbox, 'pool-a')
    assert len(await invoke(client.list_all_sandboxes)) == 1
    assert api.deletes == []
