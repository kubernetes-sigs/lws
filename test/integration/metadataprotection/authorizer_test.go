/*
Copyright 2026 The Kubernetes Authors.
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

package metadataprotection

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Record real authorizer fallthroughs after RBAC denies manage-metadata.
func countMetadataAuthorizations(t *testing.T, env *envtest.Environment) *atomic.Int64 {
	t.Helper()
	calls := &atomic.Int64{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var review authorizationv1.SubjectAccessReview
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if attrs := review.Spec.ResourceAttributes; attrs != nil && attrs.Verb == "manage-metadata" {
			calls.Add(1)
		}
		review.Status = authorizationv1.SubjectAccessReviewStatus{Denied: true}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(review); err != nil {
			t.Errorf("encoding authorization response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	config := clientcmdapi.NewConfig()
	config.Clusters["webhook"] = &clientcmdapi.Cluster{Server: server.URL, CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}
	config.AuthInfos["webhook"] = &clientcmdapi.AuthInfo{}
	config.Contexts["webhook"] = &clientcmdapi.Context{Cluster: "webhook", AuthInfo: "webhook"}
	config.CurrentContext = "webhook"
	path := filepath.Join(t.TempDir(), "authorization.kubeconfig")
	must(t, clientcmd.WriteToFile(*config, path))
	env.ControlPlane.GetAPIServer().Configure().
		Set("authorization-mode", "RBAC,Webhook").
		Set("authorization-webhook-config-file", path).
		Set("authorization-webhook-version", "v1").
		Set("authorization-webhook-cache-authorized-ttl", "0s").
		Set("authorization-webhook-cache-unauthorized-ttl", "0s")
	return calls
}
