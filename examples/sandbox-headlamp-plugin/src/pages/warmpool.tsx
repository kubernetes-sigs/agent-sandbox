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
import { DetailsGrid, ResourceListView } from '@kinvolk/headlamp-plugin/lib/components/common';
import { useParams } from 'react-router-dom';
import { SandboxWarmPool } from '../resources';
import { LinkValue } from './common';

export function SandboxWarmPoolList() {
  const { t } = useTranslation();

  return (
    <ResourceListView
      title={t('Sandbox WarmPools')}
      resourceClass={SandboxWarmPool}
      headerProps={{ titleSideActions: [] }}
      enableRowActions={false}
      enableRowSelection={false}
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
          render: item => (
            <LinkValue
              routeName="agent-sandbox-template"
              namespace={item.metadata.namespace}
              name={item.spec.sandboxTemplateRef?.name}
            />
          ),
        },
        {
          id: 'update-strategy',
          label: 'Update strategy',
          getValue: item => item.updateStrategy,
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
      noDefaultActions
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
            value: (
              <LinkValue
                routeName="agent-sandbox-template"
                namespace={item.metadata.namespace}
                name={item.spec.sandboxTemplateRef?.name}
              />
            ),
          },
          {
            name: t('Update strategy'),
            value: item.updateStrategy,
          },
        ]
      }
    />
  );
}
