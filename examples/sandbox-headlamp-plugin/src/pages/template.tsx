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
import { SandboxTemplate } from '../resources';

export function SandboxTemplateList() {
  const { t } = useTranslation();

  return (
    <ResourceListView
      title={t('Sandbox Templates')}
      resourceClass={SandboxTemplate}
      headerProps={{ titleSideActions: [] }}
      enableRowActions={false}
      enableRowSelection={false}
      columns={['name', 'namespace', 'age']}
    />
  );
}

export function SandboxTemplateDetail() {
  const { namespace, name } = useParams<{ namespace: string; name: string }>();
  const { t } = useTranslation();

  return (
    <DetailsGrid
      resourceType={SandboxTemplate}
      name={name}
      namespace={namespace}
      withEvents
      noDefaultActions
      extraInfo={item =>
        item && [
          { name: t('API version'), value: SandboxTemplate.apiVersion },
          { name: t('Kind'), value: SandboxTemplate.kind },
          {
            name: t('Network policy management'),
            value: item.networkPolicyManagement,
          },
          {
            name: t('Environment variable injection'),
            value: item.envVarsInjectionPolicy,
          },
          {
            name: t('Volume claim template injection'),
            value: item.volumeClaimTemplatesPolicy,
          },
          {
            name: t('Service'),
            value: item.serviceState,
          },
        ]
      }
    />
  );
}
