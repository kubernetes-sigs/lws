/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cert

import (
	"fmt"

	cert "github.com/open-policy-agent/cert-controller/pkg/rotator"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	validateWebhookConfName = "disaggregatedset-validating-webhook-configuration"
	caName                  = "disaggregatedset-ca"
	caOrg                   = "disaggregatedset"
)

//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;update
//+kubebuilder:rbac:groups="admissionregistration.k8s.io",resources=validatingwebhookconfigurations,verbs=get;list;watch;update

// CertsManager creates certs for webhooks.
func CertsManager(mgr ctrl.Manager, namespace string, serviceName string, secretName string, certDir string, setupFinish chan struct{}) error {
	// dnsName is the format of <service name>.<namespace>.svc
	var dnsName = fmt.Sprintf("%s.%s.svc", serviceName, namespace)

	return cert.AddRotator(mgr, &cert.CertRotator{
		SecretKey: types.NamespacedName{
			Namespace: namespace,
			Name:      secretName,
		},
		CertDir:        certDir,
		CAName:         caName,
		CAOrganization: caOrg,
		DNSName:        dnsName,
		IsReady:        setupFinish,
		Webhooks: []cert.WebhookInfo{
			{
				Type: cert.Validating,
				Name: validateWebhookConfName,
			},
		},
		EnableReadinessCheck: true,
	})
}
