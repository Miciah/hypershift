package kas

import (
	"encoding/json"
	"fmt"
	"path"

	"github.com/blang/semver"
	hyperv1 "github.com/openshift/hypershift/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	podsecurityadmissionv1beta1 "k8s.io/pod-security-admission/admission/api/v1beta1"

	configv1 "github.com/openshift/api/config/v1"
	kcpv1 "github.com/openshift/api/kubecontrolplane/v1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/cloud"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/pki"
	hcpconfig "github.com/openshift/hypershift/support/config"
	"github.com/openshift/hypershift/support/globalconfig"
)

const (
	KubeAPIServerConfigKey  = "config.json"
	OauthMetadataConfigKey  = "oauthMetadata.json"
	AuditLogFile            = "audit.log"
	EgressSelectorConfigKey = "config.yaml"
	DefaultEtcdPort         = 2379
)

func ReconcileConfig(config *corev1.ConfigMap,
	ownerRef hcpconfig.OwnerRef,
	p KubeAPIServerConfigParams,
	version semver.Version,
) error {
	ownerRef.ApplyTo(config)
	if config.Data == nil {
		config.Data = map[string]string{}
	}
	kasConfig := generateConfig(p, version)
	serializedConfig, err := json.Marshal(kasConfig)
	if err != nil {
		return fmt.Errorf("failed to serialize kube apiserver config: %w", err)
	}
	config.Data[KubeAPIServerConfigKey] = string(serializedConfig)
	return nil
}

type kubeAPIServerArgs map[string]kcpv1.Arguments

func (a kubeAPIServerArgs) Set(name string, values ...string) {
	v := kcpv1.Arguments{}
	v = append(v, values...)
	a[name] = v
}

func generateConfig(p KubeAPIServerConfigParams, version semver.Version) *kcpv1.KubeAPIServerConfig {
	cpath := func(volume, file string) string {
		return path.Join(volumeMounts.Path(kasContainerMain().Name, volume), file)
	}
	config := &kcpv1.KubeAPIServerConfig{
		TypeMeta: metav1.TypeMeta{
			Kind:       "KubeAPIServerConfig",
			APIVersion: kcpv1.GroupVersion.String(),
		},
		GenericAPIServerConfig: configv1.GenericAPIServerConfig{
			AdmissionConfig: configv1.AdmissionConfig{
				PluginConfig: map[string]configv1.AdmissionPluginConfig{
					"network.openshift.io/ExternalIPRanger": {
						Location: "",
						Configuration: runtime.RawExtension{
							Object: externalIPRangerConfig(p.ExternalIPConfig),
						},
					},
					"network.openshift.io/RestrictedEndpointsAdmission": {
						Location: "",
						Configuration: runtime.RawExtension{
							Object: restrictedEndpointsAdmission(p.ClusterNetwork, p.ServiceNetwork),
						},
					},
					"PodSecurity": {
						Configuration: runtime.RawExtension{
							Object: &podsecurityadmissionv1beta1.PodSecurityConfiguration{
								TypeMeta: metav1.TypeMeta{
									APIVersion: podsecurityadmissionv1beta1.SchemeGroupVersion.String(),
									Kind:       "PodSecurityConfiguration",
								},
								Defaults: podsecurityadmissionv1beta1.PodSecurityDefaults{
									Enforce:        "privileged",
									EnforceVersion: "latest",
									Audit:          "restricted",
									AuditVersion:   "latest",
									Warn:           "restricted",
									WarnVersion:    "latest",
								},
								Exemptions: podsecurityadmissionv1beta1.PodSecurityExemptions{
									Usernames: []string{
										"system:serviceaccount:openshift-infra:build-controller",
									},
								},
							},
						},
					},
				},
			},
			ServingInfo: configv1.HTTPServingInfo{
				ServingInfo: configv1.ServingInfo{
					CertInfo: configv1.CertInfo{
						CertFile: path.Join(volumeMounts.Path(kasContainerMain().Name, kasVolumeServerCert().Name), corev1.TLSCertKey),
						KeyFile:  path.Join(volumeMounts.Path(kasContainerMain().Name, kasVolumeServerCert().Name), corev1.TLSPrivateKeyKey),
					},
					NamedCertificates: globalconfig.GetConfigNamedCertificates(p.NamedCertificates, kasNamedCertificateMountPathPrefix),
					BindAddress:       fmt.Sprintf("0.0.0.0:%d", p.APIServerPort),
					BindNetwork:       "tcp4",
					CipherSuites:      hcpconfig.CipherSuites(p.TLSSecurityProfile),
					MinTLSVersion:     hcpconfig.MinTLSVersion(p.TLSSecurityProfile),
				},
			},
			CORSAllowedOrigins: corsAllowedOrigins(p.AdditionalCORSAllowedOrigins),
		},
		AuthConfig: kcpv1.MasterAuthConfig{
			OAuthMetadataFile: cpath(kasVolumeOauthMetadata().Name, OauthMetadataConfigKey),
		},
		ConsolePublicURL:             p.ConsolePublicURL,
		ImagePolicyConfig:            imagePolicyConfig(p.InternalRegistryHostName, p.ExternalRegistryHostNames),
		ProjectConfig:                projectConfig(p.DefaultNodeSelector),
		ServiceAccountPublicKeyFiles: []string{cpath(kasVolumeServiceAccountKey().Name, pki.ServiceSignerPublicKey)},
		ServicesSubnet:               p.ServiceNetwork,
	}
	args := kubeAPIServerArgs{}
	args.Set("advertise-address", p.AdvertiseAddress)
	args.Set("allow-privileged", "true")
	args.Set("anonymous-auth", "true")
	args.Set("api-audiences", p.ServiceAccountIssuerURL)
	args.Set("audit-log-format", "json")
	args.Set("audit-log-maxbackup", "10")
	args.Set("audit-log-maxsize", "100")
	args.Set("audit-log-path", cpath(kasVolumeWorkLogs().Name, AuditLogFile))
	args.Set("audit-policy-file", cpath(kasVolumeAuditConfig().Name, AuditPolicyConfigMapKey))
	args.Set("authentication-token-webhook-config-file", cpath(kasVolumeAuthTokenWebhookConfig().Name, KubeconfigKey))
	args.Set("authentication-token-webhook-version", "v1")
	args.Set("authorization-mode", "Scope", "SystemMasters", "RBAC", "Node")
	args.Set("client-ca-file", cpath(kasVolumeClientCA().Name, pki.CASignerCertMapKey))
	if p.CloudProviderConfigRef != nil {
		args.Set("cloud-config", cloudProviderConfig(p.CloudProviderConfigRef.Name, p.CloudProvider))
	}
	if p.CloudProvider != "" {
		args.Set("cloud-provider", p.CloudProvider)
	}
	if p.AuditWebhookEnabled {
		args.Set("audit-webhook-config-file", auditWebhookConfigFile())
		args.Set("audit-webhook-mode", "batch")
	}
	if p.DisableProfiling {
		args.Set("profiling", "false")
	}
	args.Set("egress-selector-config-file", cpath(kasVolumeEgressSelectorConfig().Name, EgressSelectorConfigMapKey))
	args.Set("enable-admission-plugins", admissionPlugins()...)
	if version.Minor == 10 {
		// This is explicitly disabled in OCP 4.10
		// This is enabled by default in 4.11 but currently disabled by OCP. It is planned to get re-enabled but currently
		// breaks conformance testing, ref: https://github.com/openshift/cluster-kube-apiserver-operator/pull/1262
		args.Set("disable-admission-plugins", "PodSecurity")
	}
	args.Set("enable-aggregator-routing", "true")
	args.Set("enable-logs-handler", "false")
	args.Set("endpoint-reconciler-type", "lease")
	args.Set("etcd-cafile", cpath(kasVolumeEtcdClientCert().Name, pki.EtcdClientCAKey))
	args.Set("etcd-certfile", cpath(kasVolumeEtcdClientCert().Name, pki.EtcdClientCrtKey))
	args.Set("etcd-keyfile", cpath(kasVolumeEtcdClientCert().Name, pki.EtcdClientKeyKey))
	args.Set("etcd-prefix", "kubernetes.io")
	args.Set("etcd-servers", p.EtcdURL)
	args.Set("event-ttl", "3h")
	args.Set("feature-gates", p.FeatureGates...)
	args.Set("goaway-chance", "0")
	args.Set("http2-max-streams-per-connection", "2000")
	args.Set("kubelet-certificate-authority", cpath(kasVolumeKubeletClientCA().Name, pki.CASignerCertMapKey))
	args.Set("kubelet-client-certificate", cpath(kasVolumeKubeletClientCert().Name, corev1.TLSCertKey))
	args.Set("kubelet-client-key", cpath(kasVolumeKubeletClientCert().Name, corev1.TLSPrivateKeyKey))
	args.Set("kubelet-preferred-address-types", "InternalIP")
	args.Set("kubelet-read-only-port", "0")
	args.Set("kubernetes-service-node-port", "0")
	args.Set("max-mutating-requests-inflight", "1000")
	args.Set("max-requests-inflight", "3000")
	args.Set("min-request-timeout", "3600")
	args.Set("proxy-client-cert-file", cpath(kasVolumeAggregatorCert().Name, corev1.TLSCertKey))
	args.Set("proxy-client-key-file", cpath(kasVolumeAggregatorCert().Name, corev1.TLSPrivateKeyKey))
	args.Set("requestheader-allowed-names", requestHeaderAllowedNames()...)
	args.Set("requestheader-client-ca-file", cpath(kasVolumeAggregatorCA().Name, pki.CASignerCertMapKey))
	args.Set("requestheader-extra-headers-prefix", "X-Remote-Extra-")
	args.Set("requestheader-group-headers", "X-Remote-Group")
	args.Set("requestheader-username-headers", "X-Remote-User")
	args.Set("runtime-config", "flowcontrol.apiserver.k8s.io/v1alpha1=true")
	args.Set("service-account-issuer", p.ServiceAccountIssuerURL)
	args.Set("service-account-jwks-uri", jwksURL(p.ServiceAccountIssuerURL))
	args.Set("service-account-lookup", "true")
	args.Set("service-account-signing-key-file", cpath(kasVolumeServiceAccountKey().Name, pki.ServiceSignerPrivateKey))
	args.Set("service-node-port-range", p.NodePortRange)
	args.Set("shutdown-delay-duration", "70s")
	args.Set("shutdown-send-retry-after", "true")
	args.Set("storage-backend", "etcd3")
	args.Set("storage-media-type", "application/vnd.kubernetes.protobuf")
	args.Set("tls-cert-file", cpath(kasVolumeServerCert().Name, corev1.TLSCertKey))
	args.Set("tls-private-key-file", cpath(kasVolumeServerCert().Name, corev1.TLSPrivateKeyKey))
	config.APIServerArguments = args
	return config
}

func cloudProviderConfig(cloudProviderConfigName, cloudProvider string) string {
	if cloudProviderConfigName != "" {
		cfgDir := cloudProviderConfigVolumeMount.Path(kasContainerMain().Name, kasVolumeCloudConfig().Name)
		return path.Join(cfgDir, cloud.ProviderConfigKey(cloudProvider))
	}
	return ""
}

func externalIPRangerConfig(externalIPConfig *configv1.ExternalIPConfig) runtime.Object {
	cfg := &unstructured.Unstructured{}
	cfg.SetAPIVersion("network.openshift.io/v1")
	cfg.SetKind("ExternalIPRangerAdmissionConfig")
	conf := []string{}
	if externalIPConfig != nil && externalIPConfig.Policy != nil {
		for _, cidr := range externalIPConfig.Policy.RejectedCIDRs {
			conf = append(conf, "!"+cidr)
		}
		conf = append(conf, externalIPConfig.Policy.AllowedCIDRs...)
	}
	unstructured.SetNestedStringSlice(cfg.Object, conf, "externalIPNetworkCIDRs")
	allowIngressIP := externalIPConfig != nil && len(externalIPConfig.AutoAssignCIDRs) > 0
	unstructured.SetNestedField(cfg.Object, allowIngressIP, "allowIngressIP")
	return cfg
}

func restrictedEndpointsAdmission(clusterNetwork, serviceNetwork string) runtime.Object {
	cfg := &unstructured.Unstructured{}
	cfg.SetAPIVersion("network.openshift.io/v1")
	cfg.SetKind("RestrictedEndpointsAdmissionConfig")
	restrictedCIDRs := []string{clusterNetwork, serviceNetwork}
	unstructured.SetNestedStringSlice(cfg.Object, restrictedCIDRs, "restrictedCIDRs")
	return cfg
}

func admissionPlugins() []string {
	return []string{
		"CertificateApproval",
		"CertificateSigning",
		"CertificateSubjectRestriction",
		"DefaultIngressClass",
		"DefaultStorageClass",
		"DefaultTolerationSeconds",
		"LimitRanger",
		"MutatingAdmissionWebhook",
		"NamespaceLifecycle",
		"NodeRestriction",
		"OwnerReferencesPermissionEnforcement",
		"PersistentVolumeClaimResize",
		"PersistentVolumeLabel",
		"PodNodeSelector",
		"PodTolerationRestriction",
		"Priority",
		"ResourceQuota",
		"RuntimeClass",
		"ServiceAccount",
		"StorageObjectInUseProtection",
		"TaintNodesByCondition",
		"ValidatingAdmissionWebhook",
		"authorization.openshift.io/RestrictSubjectBindings",
		"authorization.openshift.io/ValidateRoleBindingRestriction",
		"config.openshift.io/DenyDeleteClusterConfiguration",
		"config.openshift.io/ValidateAPIServer",
		"config.openshift.io/ValidateAuthentication",
		"config.openshift.io/ValidateConsole",
		"config.openshift.io/ValidateFeatureGate",
		"config.openshift.io/ValidateImage",
		"config.openshift.io/ValidateOAuth",
		"config.openshift.io/ValidateProject",
		"config.openshift.io/ValidateScheduler",
		"image.openshift.io/ImagePolicy",
		"network.openshift.io/ExternalIPRanger",
		"network.openshift.io/RestrictedEndpointsAdmission",
		"quota.openshift.io/ClusterResourceQuota",
		"quota.openshift.io/ValidateClusterResourceQuota",
		"route.openshift.io/IngressAdmission",
		"scheduling.openshift.io/OriginPodNodeEnvironment",
		"security.openshift.io/DefaultSecurityContextConstraints",
		"security.openshift.io/SCCExecRestrictions",
		"security.openshift.io/SecurityContextConstraint",
		"security.openshift.io/ValidateSecurityContextConstraints",
	}
}

func corsAllowedOrigins(additionalCORSAllowedOrigins []string) []string {
	corsAllowed := []string{
		"//127\\.0\\.0\\.1(:|$)",
		"//localhost(:|$)",
	}
	corsAllowed = append(corsAllowed, additionalCORSAllowedOrigins...)
	return corsAllowed
}

func imagePolicyConfig(internalRegistryHostname string, externalRegistryHostnames []string) kcpv1.KubeAPIServerImagePolicyConfig {
	cfg := kcpv1.KubeAPIServerImagePolicyConfig{
		InternalRegistryHostname:  internalRegistryHostname,
		ExternalRegistryHostnames: externalRegistryHostnames,
	}
	return cfg
}

func projectConfig(defaultNodeSelector string) kcpv1.KubeAPIServerProjectConfig {
	return kcpv1.KubeAPIServerProjectConfig{
		DefaultNodeSelector: defaultNodeSelector,
	}
}

func requestHeaderAllowedNames() []string {
	return []string{
		"kube-apiserver-proxy",
		"system:kube-apiserver-proxy",
		"system:openshift-aggregator",
	}
}

func jwksURL(issuerURL string) string {
	return fmt.Sprintf("%s/openid/v1/jwks", issuerURL)
}

func auditWebhookConfigFile() string {
	cfgDir := kasAuditWebhookConfigFileVolumeMount.Path(kasContainerMain().Name, kasAuditWebhookConfigFileVolume().Name)
	return path.Join(cfgDir, hyperv1.AuditWebhookKubeconfigKey)
}
