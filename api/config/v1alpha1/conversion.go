/*
Copyright 2025 The Kubernetes Authors.

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"

	configv1 "sigs.k8s.io/lws/api/config/v1"
)

func addConversionFuncs(scheme *runtime.Scheme) error {
	if err := scheme.AddConversionFunc((*Configuration)(nil), (*configv1.Configuration)(nil), func(a, b interface{}, scope conversion.Scope) error {
		return Convert_v1alpha1_Configuration_To_v1_Configuration(a.(*Configuration), b.(*configv1.Configuration), scope)
	}); err != nil {
		return err
	}
	if err := scheme.AddConversionFunc((*configv1.Configuration)(nil), (*Configuration)(nil), func(a, b interface{}, scope conversion.Scope) error {
		return Convert_v1_Configuration_To_v1alpha1_Configuration(a.(*configv1.Configuration), b.(*Configuration), scope)
	}); err != nil {
		return err
	}
	return nil
}

func Convert_v1alpha1_Configuration_To_v1_Configuration(in *Configuration, out *configv1.Configuration, s conversion.Scope) error {
	if in.TypeMeta.APIVersion != "" || in.TypeMeta.Kind != "" {
		out.TypeMeta = metav1.TypeMeta{
			APIVersion: configv1.GroupVersion.String(),
			Kind:       in.TypeMeta.Kind,
		}
		if out.TypeMeta.Kind == "" {
			out.TypeMeta.Kind = "Configuration"
		}
	} else {
		out.TypeMeta = metav1.TypeMeta{}
	}
	if err := Convert_v1alpha1_ControllerManager_To_v1_ControllerManager(&in.ControllerManager, &out.ControllerManager, s); err != nil {
		return err
	}
	if in.InternalCertManagement != nil {
		in, out := &in.InternalCertManagement, &out.InternalCertManagement
		*out = new(configv1.InternalCertManagement)
		if err := Convert_v1alpha1_InternalCertManagement_To_v1_InternalCertManagement(*in, *out, s); err != nil {
			return err
		}
	} else {
		out.InternalCertManagement = nil
	}
	if in.GangSchedulingManagement != nil {
		in, out := &in.GangSchedulingManagement, &out.GangSchedulingManagement
		*out = new(configv1.GangSchedulingManagement)
		if err := Convert_v1alpha1_GangSchedulingManagement_To_v1_GangSchedulingManagement(*in, *out, s); err != nil {
			return err
		}
	} else {
		out.GangSchedulingManagement = nil
	}
	if in.FeatureGates != nil {
		in, out := &in.FeatureGates, &out.FeatureGates
		*out = make(map[string]bool, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	} else {
		out.FeatureGates = nil
	}
	if in.ClientConnection != nil {
		in, out := &in.ClientConnection, &out.ClientConnection
		*out = new(configv1.ClientConnection)
		if err := Convert_v1alpha1_ClientConnection_To_v1_ClientConnection(*in, *out, s); err != nil {
			return err
		}
	} else {
		out.ClientConnection = nil
	}
	return nil
}

func Convert_v1_Configuration_To_v1alpha1_Configuration(in *configv1.Configuration, out *Configuration, s conversion.Scope) error {
	if in.TypeMeta.APIVersion != "" || in.TypeMeta.Kind != "" {
		out.TypeMeta = metav1.TypeMeta{
			APIVersion: GroupVersion.String(),
			Kind:       in.TypeMeta.Kind,
		}
		if out.TypeMeta.Kind == "" {
			out.TypeMeta.Kind = "Configuration"
		}
	} else {
		out.TypeMeta = metav1.TypeMeta{}
	}
	if err := Convert_v1_ControllerManager_To_v1alpha1_ControllerManager(&in.ControllerManager, &out.ControllerManager, s); err != nil {
		return err
	}
	if in.InternalCertManagement != nil {
		in, out := &in.InternalCertManagement, &out.InternalCertManagement
		*out = new(InternalCertManagement)
		if err := Convert_v1_InternalCertManagement_To_v1alpha1_InternalCertManagement(*in, *out, s); err != nil {
			return err
		}
	} else {
		out.InternalCertManagement = nil
	}
	if in.GangSchedulingManagement != nil {
		in, out := &in.GangSchedulingManagement, &out.GangSchedulingManagement
		*out = new(GangSchedulingManagement)
		if err := Convert_v1_GangSchedulingManagement_To_v1alpha1_GangSchedulingManagement(*in, *out, s); err != nil {
			return err
		}
	} else {
		out.GangSchedulingManagement = nil
	}
	if in.FeatureGates != nil {
		in, out := &in.FeatureGates, &out.FeatureGates
		*out = make(map[string]bool, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	} else {
		out.FeatureGates = nil
	}
	if in.ClientConnection != nil {
		in, out := &in.ClientConnection, &out.ClientConnection
		*out = new(ClientConnection)
		if err := Convert_v1_ClientConnection_To_v1alpha1_ClientConnection(*in, *out, s); err != nil {
			return err
		}
	} else {
		out.ClientConnection = nil
	}
	return nil
}

func Convert_v1alpha1_ControllerManager_To_v1_ControllerManager(in *ControllerManager, out *configv1.ControllerManager, s conversion.Scope) error {
	if err := Convert_v1alpha1_ControllerWebhook_To_v1_ControllerWebhook(&in.Webhook, &out.Webhook, s); err != nil {
		return err
	}
	if in.LeaderElection != nil {
		out.LeaderElection = in.LeaderElection.DeepCopy()
	} else {
		out.LeaderElection = nil
	}
	if err := Convert_v1alpha1_ControllerMetrics_To_v1_ControllerMetrics(&in.Metrics, &out.Metrics, s); err != nil {
		return err
	}
	if err := Convert_v1alpha1_ControllerHealth_To_v1_ControllerHealth(&in.Health, &out.Health, s); err != nil {
		return err
	}
	if in.TLS != nil {
		in, out := &in.TLS, &out.TLS
		*out = new(configv1.TLSOptions)
		if err := Convert_v1alpha1_TLSOptions_To_v1_TLSOptions(*in, *out, s); err != nil {
			return err
		}
	} else {
		out.TLS = nil
	}
	return nil
}

func Convert_v1_ControllerManager_To_v1alpha1_ControllerManager(in *configv1.ControllerManager, out *ControllerManager, s conversion.Scope) error {
	if err := Convert_v1_ControllerWebhook_To_v1alpha1_ControllerWebhook(&in.Webhook, &out.Webhook, s); err != nil {
		return err
	}
	if in.LeaderElection != nil {
		out.LeaderElection = in.LeaderElection.DeepCopy()
	} else {
		out.LeaderElection = nil
	}
	if err := Convert_v1_ControllerMetrics_To_v1alpha1_ControllerMetrics(&in.Metrics, &out.Metrics, s); err != nil {
		return err
	}
	if err := Convert_v1_ControllerHealth_To_v1alpha1_ControllerHealth(&in.Health, &out.Health, s); err != nil {
		return err
	}
	if in.TLS != nil {
		in, out := &in.TLS, &out.TLS
		*out = new(TLSOptions)
		if err := Convert_v1_TLSOptions_To_v1alpha1_TLSOptions(*in, *out, s); err != nil {
			return err
		}
	} else {
		out.TLS = nil
	}
	return nil
}

func Convert_v1alpha1_ControllerWebhook_To_v1_ControllerWebhook(in *ControllerWebhook, out *configv1.ControllerWebhook, s conversion.Scope) error {
	if in.Port != nil {
		in, out := &in.Port, &out.Port
		*out = new(int)
		**out = **in
	} else {
		out.Port = nil
	}
	out.Host = in.Host
	out.CertDir = in.CertDir
	return nil
}

func Convert_v1_ControllerWebhook_To_v1alpha1_ControllerWebhook(in *configv1.ControllerWebhook, out *ControllerWebhook, s conversion.Scope) error {
	if in.Port != nil {
		in, out := &in.Port, &out.Port
		*out = new(int)
		**out = **in
	} else {
		out.Port = nil
	}
	out.Host = in.Host
	out.CertDir = in.CertDir
	return nil
}

func Convert_v1alpha1_ControllerMetrics_To_v1_ControllerMetrics(in *ControllerMetrics, out *configv1.ControllerMetrics, s conversion.Scope) error {
	out.BindAddress = in.BindAddress
	return nil
}

func Convert_v1_ControllerMetrics_To_v1alpha1_ControllerMetrics(in *configv1.ControllerMetrics, out *ControllerMetrics, s conversion.Scope) error {
	out.BindAddress = in.BindAddress
	return nil
}

func Convert_v1alpha1_ControllerHealth_To_v1_ControllerHealth(in *ControllerHealth, out *configv1.ControllerHealth, s conversion.Scope) error {
	out.HealthProbeBindAddress = in.HealthProbeBindAddress
	out.ReadinessEndpointName = in.ReadinessEndpointName
	out.LivenessEndpointName = in.LivenessEndpointName
	return nil
}

func Convert_v1_ControllerHealth_To_v1alpha1_ControllerHealth(in *configv1.ControllerHealth, out *ControllerHealth, s conversion.Scope) error {
	out.HealthProbeBindAddress = in.HealthProbeBindAddress
	out.ReadinessEndpointName = in.ReadinessEndpointName
	out.LivenessEndpointName = in.LivenessEndpointName
	return nil
}

func Convert_v1alpha1_InternalCertManagement_To_v1_InternalCertManagement(in *InternalCertManagement, out *configv1.InternalCertManagement, s conversion.Scope) error {
	if in.Enable != nil {
		in, out := &in.Enable, &out.Enable
		*out = new(bool)
		**out = **in
	} else {
		out.Enable = nil
	}
	if in.WebhookServiceName != nil {
		in, out := &in.WebhookServiceName, &out.WebhookServiceName
		*out = new(string)
		**out = **in
	} else {
		out.WebhookServiceName = nil
	}
	if in.WebhookSecretName != nil {
		in, out := &in.WebhookSecretName, &out.WebhookSecretName
		*out = new(string)
		**out = **in
	} else {
		out.WebhookSecretName = nil
	}
	return nil
}

func Convert_v1_InternalCertManagement_To_v1alpha1_InternalCertManagement(in *configv1.InternalCertManagement, out *InternalCertManagement, s conversion.Scope) error {
	if in.Enable != nil {
		in, out := &in.Enable, &out.Enable
		*out = new(bool)
		**out = **in
	} else {
		out.Enable = nil
	}
	if in.WebhookServiceName != nil {
		in, out := &in.WebhookServiceName, &out.WebhookServiceName
		*out = new(string)
		**out = **in
	} else {
		out.WebhookServiceName = nil
	}
	if in.WebhookSecretName != nil {
		in, out := &in.WebhookSecretName, &out.WebhookSecretName
		*out = new(string)
		**out = **in
	} else {
		out.WebhookSecretName = nil
	}
	return nil
}

func Convert_v1alpha1_ClientConnection_To_v1_ClientConnection(in *ClientConnection, out *configv1.ClientConnection, s conversion.Scope) error {
	if in.QPS != nil {
		in, out := &in.QPS, &out.QPS
		*out = new(float32)
		**out = **in
	} else {
		out.QPS = nil
	}
	if in.Burst != nil {
		in, out := &in.Burst, &out.Burst
		*out = new(int32)
		**out = **in
	} else {
		out.Burst = nil
	}
	return nil
}

func Convert_v1_ClientConnection_To_v1alpha1_ClientConnection(in *configv1.ClientConnection, out *ClientConnection, s conversion.Scope) error {
	if in.QPS != nil {
		in, out := &in.QPS, &out.QPS
		*out = new(float32)
		**out = **in
	} else {
		out.QPS = nil
	}
	if in.Burst != nil {
		in, out := &in.Burst, &out.Burst
		*out = new(int32)
		**out = **in
	} else {
		out.Burst = nil
	}
	return nil
}

func Convert_v1alpha1_GangSchedulingManagement_To_v1_GangSchedulingManagement(in *GangSchedulingManagement, out *configv1.GangSchedulingManagement, s conversion.Scope) error {
	if in.SchedulerProvider != nil {
		in, out := &in.SchedulerProvider, &out.SchedulerProvider
		*out = new(string)
		**out = **in
	} else {
		out.SchedulerProvider = nil
	}
	return nil
}

func Convert_v1_GangSchedulingManagement_To_v1alpha1_GangSchedulingManagement(in *configv1.GangSchedulingManagement, out *GangSchedulingManagement, s conversion.Scope) error {
	if in.SchedulerProvider != nil {
		in, out := &in.SchedulerProvider, &out.SchedulerProvider
		*out = new(string)
		**out = **in
	} else {
		out.SchedulerProvider = nil
	}
	return nil
}

func Convert_v1alpha1_TLSOptions_To_v1_TLSOptions(in *TLSOptions, out *configv1.TLSOptions, s conversion.Scope) error {
	out.MinVersion = in.MinVersion
	if in.CipherSuites != nil {
		in, out := &in.CipherSuites, &out.CipherSuites
		*out = make([]string, len(*in))
		copy(*out, *in)
	} else {
		out.CipherSuites = nil
	}
	return nil
}

func Convert_v1_TLSOptions_To_v1alpha1_TLSOptions(in *configv1.TLSOptions, out *TLSOptions, s conversion.Scope) error {
	out.MinVersion = in.MinVersion
	if in.CipherSuites != nil {
		in, out := &in.CipherSuites, &out.CipherSuites
		*out = make([]string, len(*in))
		copy(*out, *in)
	} else {
		out.CipherSuites = nil
	}
	return nil
}
