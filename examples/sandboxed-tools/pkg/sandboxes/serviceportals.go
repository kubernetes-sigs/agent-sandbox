// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sandboxes

import (
	corev1 "k8s.io/api/core/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

// ServiceConfig defines configuration for a single proxied service.
type ServiceConfig struct {
	Name       string
	TargetURL  string
	Host       string
	AuthHeader string

	// The key inside the Secret containing the auth token
	SecretKey string
}

// ServicePortalConfig defines the options for configuring the service-portal.
type ServicePortalConfig struct {
	// SecretName is the name of the Kubernetes Secret containing the API tokens.
	// Defaults to "service-portal-configuration" if empty.
	SecretName string

	// InitImage is the image of the init container configuring iptables rules.
	// Defaults to "images.local/init-iptables:latest" if empty.
	InitImage string

	// SidecarImage is the image of the service-portal proxy sidecar.
	// Defaults to "images.local/all-in-one-portal:latest" if empty.
	SidecarImage string

	// // Services is a list of services to proxy.
	// // If empty, defaults to DefaultServices.
	// Services []ServiceConfig
}

// Can create a secret using
// kubectl create secret generic service-portal-configuration   \
//     --from-literal=GEMINI_API_KEY="${GEMINI_API_KEY}$"  \
//     --from-literal=OPENAI_API_KEY="" \
//     --from-literal=GITHUB_TOKEN=$(gh auth token)

// // DefaultServices returns the default service-portal service configurations for Gemini and GitHub.
// var DefaultServices = []ServiceConfig{
// 	{
// 		Name:       "gemini",
// 		TargetURL:  "https://generativelanguage.googleapis.com",
// 		Host:       "gemini.backend",
// 		AuthHeader: "Authorization",
// 		SecretKey:  "GEMINI_API_KEY",
// 	},
// 	{
// 		Name:       "github",
// 		TargetURL:  "https://api.github.com",
// 		Host:       "github.backend",
// 		AuthHeader: "Authorization",
// 		SecretKey:  "GITHUB_TOKEN",
// 	},
// }

// AddServicePortal injects the iptables initContainer, the service-portal proxy sidecar,
// and configures hostAliases and securityContext on a Sandbox resource.
func AddServicePortal(sb *sandboxv1beta1.Sandbox, config ServicePortalConfig) {
	if config.SecretName == "" {
		config.SecretName = "service-portal-configuration"
	}
	if config.InitImage == "" {
		config.InitImage = "images.local/init-service-portals:latest"
	}
	if config.SidecarImage == "" {
		config.SidecarImage = "images.local/all-in-one-portal:latest"
	}
	// if len(config.Services) == 0 {
	// 	config.Services = DefaultServices
	// }

	podSpec := &sb.Spec.PodTemplate.Spec

	// 1. Add HostAliases
	// var hostnames []string
	// for _, svc := range config.Services {
	// 	hostnames = append(hostnames, svc.Host)
	// }
	// podSpec.HostAliases = append(podSpec.HostAliases, corev1.HostAlias{
	// 	IP:        "8.8.8.8",
	// 	Hostnames: hostnames,
	// })

	// 2. Add InitContainer
	rootUser := int64(0)
	rootGroup := int64(0)
	initContainer := corev1.Container{
		Name:            "init-iptables",
		Image:           config.InitImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env: []corev1.EnvVar{
			{
				Name:  "PROXY_PORT",
				Value: "8080",
			},
			{
				Name:  "PROXY_HTTPS_PORT",
				Value: "8443",
			},
			{
				Name:  "PROXY_UID",
				Value: "1337",
			},
			{
				Name:  "INTERCEPT_PORTS",
				Value: "80,443",
			},
			{
				Name:  "CHOWN_UID",
				Value: "1337",
			},
			{
				Name:  "CHOWN_GID",
				Value: "1337",
			},
		},
		SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{"NET_ADMIN"},
			},
			RunAsUser:  &rootUser,
			RunAsGroup: &rootGroup,
			Privileged: new(false),
		},
	}

	initContainer.VolumeMounts = []corev1.VolumeMount{
		{
			Name:      "ca-cert",
			MountPath: "/etc/service-portal/ca",
		},
		{
			Name:      "ca-key",
			MountPath: "/etc/service-portal/ca-private",
		},
	}
	podSpec.InitContainers = append(podSpec.InitContainers, initContainer)

	// 3. Prepare Sidecar Container
	var sidecarEnv []corev1.EnvVar
	// var serviceNames []string
	// for _, svc := range config.Services {
	// 	serviceNames = append(serviceNames, svc.Name)
	// }
	// sidecarEnv = append(sidecarEnv, corev1.EnvVar{
	// 	Name:  "SERVICE_NAMES",
	// 	Value: strings.Join(serviceNames, ","),
	// })

	// for _, svc := range config.Services {
	// 	prefix := strings.ToUpper(svc.Name)
	// 	sidecarEnv = append(sidecarEnv, corev1.EnvVar{
	// 		Name:  prefix + "_TARGET_URL",
	// 		Value: svc.TargetURL,
	// 	})
	// 	sidecarEnv = append(sidecarEnv, corev1.EnvVar{
	// 		Name:  prefix + "_HOST",
	// 		Value: svc.Host,
	// 	})
	// 	sidecarEnv = append(sidecarEnv, corev1.EnvVar{
	// 		Name:  prefix + "_AUTH_HEADER",
	// 		Value: svc.AuthHeader,
	// 	})
	// 	sidecarEnv = append(sidecarEnv, corev1.EnvVar{
	// 		Name: prefix + "_AUTH_TOKEN",
	// 		ValueFrom: &corev1.EnvVarSource{
	// 			SecretKeyRef: &corev1.SecretKeySelector{
	// 				LocalObjectReference: corev1.LocalObjectReference{
	// 					Name: config.SecretName,
	// 				},
	// 				Key: svc.SecretKey,
	// 			},
	// 		},
	// 	})
	// }

	sidecarEnv = append(sidecarEnv, corev1.EnvVar{
		Name:  "RULES_DIR",
		Value: "/secrets/service-portals-config",
	})

	sidecarEnv = append(sidecarEnv, corev1.EnvVar{
		Name:  "CA_CERT_PATH",
		Value: "/etc/service-portal/ca-private/tls.crt",
	})
	sidecarEnv = append(sidecarEnv, corev1.EnvVar{
		Name:  "CA_KEY_PATH",
		Value: "/etc/service-portal/ca-private/tls.key",
	})

	proxyUser := int64(1337)
	proxyGroup := int64(1337)
	falseVal := false
	trueVal := true
	sidecarContainer := corev1.Container{
		Name:  "service-portal-sidecar",
		Image: config.SidecarImage,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                &proxyUser,
			RunAsGroup:               &proxyGroup,
			AllowPrivilegeEscalation: &falseVal,
			ReadOnlyRootFilesystem:   &trueVal,
		},
		Ports: []corev1.ContainerPort{
			{
				ContainerPort: 8080,
				Name:          "proxy",
			},
		},
		Env: sidecarEnv,
	}

	sidecarContainer.VolumeMounts = append(sidecarContainer.VolumeMounts, corev1.VolumeMount{
		Name:      "service-portals-config",
		MountPath: "/secrets/service-portals-config",
		ReadOnly:  true,
	})

	sidecarContainer.VolumeMounts = append(sidecarContainer.VolumeMounts, corev1.VolumeMount{
		Name:      "ca-key",
		MountPath: "/etc/service-portal/ca-private",
		ReadOnly:  true,
	})

	podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
		Name: "service-portals-config",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: config.SecretName,
			},
		},
	})

	podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
		Name: "ca-cert",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium: corev1.StorageMediumMemory,
			},
		},
	})

	podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
		Name: "ca-key",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium: corev1.StorageMediumMemory,
			},
		},
	})

	podSpec.Containers = append(podSpec.Containers, sidecarContainer)

	// 4. Update main/sandbox container's securityContext as a best practice
	for i, container := range podSpec.Containers {
		if container.Name == "sandbox" {
			container := &podSpec.Containers[i]
			if container.SecurityContext == nil {
				container.SecurityContext = &corev1.SecurityContext{}
			}
			container.SecurityContext.RunAsNonRoot = &trueVal
			if container.SecurityContext.RunAsUser == nil {
				userVal := int64(1000)
				container.SecurityContext.RunAsUser = &userVal
			}
			container.SecurityContext.AllowPrivilegeEscalation = &falseVal

			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
				Name:      "ca-cert",
				MountPath: "/etc/service-portal/ca",
				ReadOnly:  true,
			})
		}
	}
}
