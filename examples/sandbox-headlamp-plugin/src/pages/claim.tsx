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
  ResourceListView,
} from '@kinvolk/headlamp-plugin/lib/components/common';
import { useParams } from 'react-router-dom';
import { SandboxClaim } from '../resources';
import { LinkValue } from './common';

export function SandboxClaimList() {
  const { t } = useTranslation();

  return (
    <ResourceListView
      title={t('Sandbox Claims')}
      resourceClass={SandboxClaim}
      headerProps={{ titleSideActions: [] }}
      enableRowActions={false}
      enableRowSelection={false}
      columns={[
        'name',
        'namespace',
        {
          id: 'ready',
          label: 'Ready',
          getValue: item => item.readyStatus,
        },
        {
          id: 'warm-pool',
          label: 'WarmPool',
          getValue: item => item.spec.warmPoolRef?.name || '-',
          render: item => (
            <LinkValue
              routeName="agent-sandbox-warmpool"
              namespace={item.metadata.namespace}
              name={item.spec.warmPoolRef?.name}
            />
          ),
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
      noDefaultActions
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
