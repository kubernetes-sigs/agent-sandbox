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

import { registerRoute, registerSidebarEntry, useTranslation } from '@kinvolk/headlamp-plugin/lib';
import { type ComponentType, useEffect } from 'react';
import {
  SandboxClaimDetail,
  SandboxClaimList,
  SandboxDetail,
  SandboxList,
  SandboxTemplateDetail,
  SandboxTemplateList,
  SandboxWarmPoolDetail,
  SandboxWarmPoolList,
} from './pages';

registerSidebarEntry({
  name: 'agent-sandbox',
  label: 'Agent Sandbox',
  url: '/agent-sandbox/sandboxes',
  icon: 'mdi:robot-outline',
  parent: '',
});

const resources = [
  {
    name: 'agent-sandbox-sandboxes',
    label: 'Sandboxes',
    translationKey: 'Sandboxes',
    path: 'sandboxes',
    list: SandboxList,
    detail: SandboxDetail,
    detailName: 'agent-sandbox-sandbox',
  },
  {
    name: 'agent-sandbox-claims',
    label: 'Sandbox Claims',
    translationKey: 'Sandbox Claims',
    path: 'claims',
    list: SandboxClaimList,
    detail: SandboxClaimDetail,
    detailName: 'agent-sandbox-claim',
  },
  {
    name: 'agent-sandbox-warmpools',
    label: 'Sandbox WarmPools',
    translationKey: 'Sandbox WarmPools',
    path: 'warmpools',
    list: SandboxWarmPoolList,
    detail: SandboxWarmPoolDetail,
    detailName: 'agent-sandbox-warmpool',
  },
  {
    name: 'agent-sandbox-templates',
    label: 'Sandbox Templates',
    translationKey: 'Sandbox Templates',
    path: 'templates',
    list: SandboxTemplateList,
    detail: SandboxTemplateDetail,
    detailName: 'agent-sandbox-template',
  },
];

function registerLocalizedSidebarEntries(t: (key: string) => string) {
  registerSidebarEntry({
    name: 'agent-sandbox',
    label: t('Agent Sandbox'),
    url: '/agent-sandbox/sandboxes',
    icon: 'mdi:robot-outline',
    parent: '',
  });

  for (const resource of resources) {
    registerSidebarEntry({
      name: resource.name,
      label: t(resource.translationKey),
      url: `/agent-sandbox/${resource.path}`,
      parent: 'agent-sandbox',
    });
  }
}

function withSidebarTranslations(Component: ComponentType<any>) {
  return function LocalizedPage() {
    const { t, i18n } = useTranslation();

    useEffect(() => {
      registerLocalizedSidebarEntries(t);
    }, [t, i18n?.language]);

    return <Component />;
  };
}

for (const resource of resources) {
  registerSidebarEntry({
    name: resource.name,
    label: resource.label,
    url: `/agent-sandbox/${resource.path}`,
    parent: 'agent-sandbox',
  });

  registerRoute({
    path: `/agent-sandbox/${resource.path}`,
    sidebar: resource.name,
    name: resource.name,
    exact: true,
    component: withSidebarTranslations(resource.list),
  });

  registerRoute({
    path: `/agent-sandbox/${resource.path}/:namespace/:name`,
    sidebar: resource.name,
    name: resource.detailName,
    exact: true,
    component: withSidebarTranslations(resource.detail),
  });
}
