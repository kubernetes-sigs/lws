/*
Copyright 2025.

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

package podgroup

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	PodGroupNameFmt = "%s-%s"
)

// GetPodGroupName returns the name of the PodGroup for a given LeaderWorkloadSet name and group index.
func GetPodGroupName(lwsName, groupIndex string) string {
	return fmt.Sprintf(PodGroupNameFmt, lwsName, groupIndex)
}

// ProviderType defines the type of PodGroup provider
type ProviderType string

const (
	Volcano ProviderType = "volcano"
)

// NewPodGroupProvider creates a new PodGroup provider based on the type
func NewPodGroupProvider(providerType ProviderType, client client.Client) (Provider, error) {
	switch providerType {
	case Volcano:
		return NewVolcanoProvider(client), nil
	default:
		return nil, fmt.Errorf("unsupported provider type: %s", providerType)
	}
}
