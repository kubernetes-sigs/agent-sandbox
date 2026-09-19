/*
 * Copyright 2026 The Kubernetes Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { useTranslation } from '@kinvolk/headlamp-plugin/lib';
import {
  ConditionsSection,
  DetailsGrid,
  Link,
  NameValueTable,
  ResourceListView,
  SectionBox,
} from '@kinvolk/headlamp-plugin/lib/components/common';
import { useParams } from 'react-router-dom';
import { Sandbox, SandboxClaim, SandboxWarmPool } from './resources';

function conditionValue(resource: {
  status?: { conditions?: Array<{ type: string; status: string }> };
}) {
  return resource.status?.conditions?.find(condition => condition.type === 'Ready')?.status || '-';
}

function LinkValue({
  routeName,
  namespace,
  name,
}: {
  routeName: string;
  namespace?: string;
  name?: string;
}) {
  if (!name || name === '-') {
    return <>-</>;
  }

  return (
    <Link routeName={routeName} params={{ namespace, name }}>
      {name}
    </Link>
  );
}

export function SandboxList() {
  const { t } = useTranslation();

  return (
    <ResourceListView
      title={t('Sandboxes')}
      resourceClass={Sandbox}
      columns={[
        'name',
        'namespace',
        {
          id: 'ready',
          label: 'Ready',
          getValue: item => item.readyStatus,
        },
        {
          id: 'launch-type',
          label: 'Launch type',
          getValue: item => item.launchType,
        },
        {
          id: 'pod-ip',
          label: 'Pod IP',
          getValue: item => item.podIP,
        },
        {
          id: 'node',
          label: 'Node',
          getValue: item => item.status.nodeName || '-',
        },
        'age',
      ]}
    />
  );
}

export function SandboxDetail() {
  const { namespace, name } = useParams<{ namespace: string; name: string }>();
  const { t } = useTranslation();

  return (
    <DetailsGrid
      resourceType={Sandbox}
      name={name}
      namespace={namespace}
      withEvents
      extraInfo={item =>
        item && [
          { name: t('API version'), value: Sandbox.apiVersion },
          { name: t('Kind'), value: Sandbox.kind },
          { name: t('Ready'), value: item.readyStatus },
          { name: t('Ready reason'), value: item.readyCondition?.reason || '-' },
          { name: t('Operating mode'), value: item.spec.operatingMode || 'Running' },
          { name: t('Launch type'), value: item.launchType },
          { name: t('Pod IPs'), value: item.podIP },
          { name: t('Node'), value: item.status.nodeName || '-' },
          { name: t('Service FQDN'), value: item.status.serviceFQDN || '-' },
        ]
      }
      extraSections={item =>
        item && [
          {
            id: 'agent-sandbox-related-resources',
            section: (
              <SectionBox title={t('Related resources')}>
                <NameValueTable
                  rows={[
                    {
                      name: t('Backing Pod'),
                      value: (
                        <LinkValue
                          routeName="pod"
                          namespace={item.metadata.namespace}
                          name={item.backingPodName}
                        />
                      ),
                    },
                    {
                      name: t('Service'),
                      value: (
                        <LinkValue
                          routeName="service"
                          namespace={item.metadata.namespace}
                          name={item.status.service}
                        />
                      ),
                    },
                  ]}
                />
              </SectionBox>
            ),
          },
          {
            id: 'agent-sandbox-conditions',
            section: <ConditionsSection resource={item.jsonData} />,
          },
        ]
      }
    />
  );
}

export function SandboxClaimList() {
  const { t } = useTranslation();

  return (
    <ResourceListView
      title={t('Sandbox Claims')}
      resourceClass={SandboxClaim}
      columns={[
        'name',
        'namespace',
        {
          id: 'ready',
          label: 'Ready',
          getValue: item => conditionValue(item.jsonData),
        },
        {
          id: 'warm-pool',
          label: 'WarmPool',
          getValue: item => item.spec.warmPoolRef?.name || '-',
        },
        {
          id: 'sandbox',
          label: 'Sandbox',
          getValue: item => item.assignedSandboxName,
          render: item => (
            <LinkValue
              routeName="agent-sandbox-sandbox"
              namespace={item.metadata.namespace}
              name={item.status.sandbox?.name}
            />
          ),
        },
        'age',
      ]}
    />
  );
}

export function SandboxClaimDetail() {
  const { namespace, name } = useParams<{ namespace: string; name: string }>();
  const { t } = useTranslation();

  return (
    <DetailsGrid
      resourceType={SandboxClaim}
      name={name}
      namespace={namespace}
      withEvents
      extraInfo={item =>
        item && [
          { name: t('API version'), value: SandboxClaim.apiVersion },
          { name: t('Kind'), value: SandboxClaim.kind },
          {
            name: t('WarmPool'),
            value: (
              <LinkValue
                routeName="agent-sandbox-warmpool"
                namespace={item.metadata.namespace}
                name={item.spec.warmPoolRef?.name}
              />
            ),
          },
          {
            name: t('Assigned Sandbox'),
            value: (
              <LinkValue
                routeName="agent-sandbox-sandbox"
                namespace={item.metadata.namespace}
                name={item.status.sandbox?.name}
              />
            ),
          },
          { name: t('Pod IPs'), value: item.status.sandbox?.podIPs?.join(', ') || '-' },
          { name: t('Service FQDN'), value: item.status.sandbox?.serviceFQDN || '-' },
          {
            name: t('Shutdown time'),
            value: item.jsonData.spec.lifecycle?.shutdownTime || '-',
          },
        ]
      }
      extraSections={item =>
        item && [
          {
            id: 'agent-sandbox-claim-conditions',
            section: <ConditionsSection resource={item.jsonData} />,
          },
        ]
      }
    />
  );
}

export function SandboxWarmPoolList() {
  const { t } = useTranslation();

  return (
    <ResourceListView
      title={t('Sandbox WarmPools')}
      resourceClass={SandboxWarmPool}
      columns={[
        'name',
        'namespace',
        {
          id: 'ready',
          label: 'Ready replicas',
          getValue: item => `${item.readyReplicas} / ${item.desiredReplicas}`,
        },
        {
          id: 'template',
          label: 'Template',
          getValue: item => item.spec.sandboxTemplateRef?.name || '-',
        },
        {
          id: 'update-strategy',
          label: 'Update strategy',
          getValue: item => item.spec.updateStrategy?.type || 'OnReplenish',
        },
        'age',
      ]}
    />
  );
}

export function SandboxWarmPoolDetail() {
  const { namespace, name } = useParams<{ namespace: string; name: string }>();
  const { t } = useTranslation();

  return (
    <DetailsGrid
      resourceType={SandboxWarmPool}
      name={name}
      namespace={namespace}
      withEvents
      extraInfo={item =>
        item && [
          { name: t('API version'), value: SandboxWarmPool.apiVersion },
          { name: t('Kind'), value: SandboxWarmPool.kind },
          { name: t('Desired replicas'), value: item.desiredReplicas },
          { name: t('Current replicas'), value: item.status.replicas ?? 0 },
          { name: t('Ready replicas'), value: item.readyReplicas },
          { name: t('Selector'), value: item.status.selector || '-' },
          {
            name: t('Template'),
            value: item.spec.sandboxTemplateRef?.name || '-',
          },
          {
            name: t('Update strategy'),
            value: item.jsonData.spec.updateStrategy?.type || 'OnReplenish',
          },
        ]
      }
    />
  );
}
