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
  NameValueTable,
  ResourceListView,
  SectionBox,
} from '@kinvolk/headlamp-plugin/lib/components/common';
import { useParams } from 'react-router-dom';
import { Sandbox } from '../resources';
import { LinkValue } from './common';

export function SandboxList() {
  const { t } = useTranslation();

  return (
    <ResourceListView
      title={t('Sandboxes')}
      resourceClass={Sandbox}
      headerProps={{ titleSideActions: [] }}
      enableRowActions={false}
      enableRowSelection={false}
      columns={[
        'name',
        'namespace',
        {
          id: 'ready',
          label: t('Ready'),
          getValue: item => item.readyStatus,
        },
        {
          id: 'launch-type',
          label: t('Launch type'),
          getValue: item => item.launchType,
        },
        {
          id: 'pod-ip',
          label: t('Pod IP'),
          getValue: item => item.podIP,
        },
        {
          id: 'node',
          label: t('Node'),
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
      noDefaultActions
      extraInfo={item =>
        item && [
          { name: t('API version'), value: Sandbox.apiVersion },
          { name: t('Kind'), value: Sandbox.kind },
          { name: t('Ready'), value: item.readyStatus },
          { name: t('Ready reason'), value: item.readyCondition?.reason || '-' },
          { name: t('Operating mode'), value: item.operatingMode },
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
