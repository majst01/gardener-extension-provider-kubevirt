// Copyright (c) 2020 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package validator

import (
	"context"
	"fmt"
	"reflect"

	"github.com/gardener/gardener-extension-provider-kubevirt/pkg/admission"
	apiskubevirt "github.com/gardener/gardener-extension-provider-kubevirt/pkg/apis/kubevirt"
	"github.com/gardener/gardener-extension-provider-kubevirt/pkg/apis/kubevirt/validation"

	extensionswebhook "github.com/gardener/gardener/extensions/pkg/webhook"
	"github.com/gardener/gardener/pkg/apis/core"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type shoot struct {
	client         client.Client
	apiReader      client.Reader
	decoder        runtime.Decoder
	lenientDecoder runtime.Decoder
}

// NewShootValidator returns a new instance of a shoot validator.
func NewShootValidator() extensionswebhook.Validator {
	return &shoot{}
}

// InjectScheme injects the given scheme into the validator.
func (s *shoot) InjectScheme(scheme *runtime.Scheme) error {
	s.decoder = serializer.NewCodecFactory(scheme, serializer.EnableStrict).UniversalDecoder()
	s.lenientDecoder = serializer.NewCodecFactory(scheme).UniversalDecoder()
	return nil
}

// InjectClient injects the given client into the validator.
func (s *shoot) InjectClient(client client.Client) error {
	s.client = client
	return nil
}

// InjectAPIReader injects the given apiReader into the validator.
func (s *shoot) InjectAPIReader(apiReader client.Reader) error {
	s.apiReader = apiReader
	return nil
}

// Validate validates the given shoot objects.
func (s *shoot) Validate(ctx context.Context, new, old client.Object) error {
	shoot, ok := new.(*core.Shoot)
	if !ok {
		return fmt.Errorf("wrong object type %T", new)
	}

	if old != nil {
		oldShoot, ok := old.(*core.Shoot)
		if !ok {
			return fmt.Errorf("wrong object type %T for old object", old)
		}
		return s.validateUpdate(ctx, oldShoot, shoot)
	}

	return s.validateCreate(ctx, shoot)
}

var (
	specPath = field.NewPath("spec")

	networkPath  = specPath.Child("networking")
	providerPath = specPath.Child("provider")

	infrastructureConfigPath = providerPath.Child("infrastructureConfig")
	controlPlaneConfigPath   = providerPath.Child("controlPlaneConfig")
	workersPath              = providerPath.Child("workers")
	workerConfigPath         = func(index int) *field.Path { return workersPath.Index(index).Child("providerConfig") }
)

type workerConfigContext struct {
	workerConfig *apiskubevirt.WorkerConfig
	dataVolumes  []core.DataVolume
}

type validationContext struct {
	shoot                 *core.Shoot
	infrastructureConfig  *apiskubevirt.InfrastructureConfig
	controlPlaneConfig    *apiskubevirt.ControlPlaneConfig
	workerConfigsContexts []*workerConfigContext
	cloudProfile          *gardencorev1beta1.CloudProfile
	cloudProfileConfig    *apiskubevirt.CloudProfileConfig
}

func (s *shoot) validateContext(valContext *validationContext) field.ErrorList {
	var (
		allErrors = field.ErrorList{}
	)

	allErrors = append(allErrors, validation.ValidateNetworking(valContext.shoot.Spec.Networking, networkPath)...)
	allErrors = append(allErrors, validation.ValidateInfrastructureConfig(valContext.infrastructureConfig, infrastructureConfigPath)...)
	allErrors = append(allErrors, validation.ValidateControlPlaneConfig(valContext.controlPlaneConfig, valContext.shoot.Spec.Kubernetes.Version, controlPlaneConfigPath)...)
	allErrors = append(allErrors, validation.ValidateWorkers(valContext.shoot.Spec.Provider.Workers, workersPath)...)
	for i, workerConfigContext := range valContext.workerConfigsContexts {
		allErrors = append(allErrors, validation.ValidateWorkerConfig(workerConfigContext.workerConfig, workerConfigContext.dataVolumes, workerConfigPath(i))...)
	}

	return allErrors
}

func (s *shoot) validateCreate(ctx context.Context, shoot *core.Shoot) error {
	validationContext, err := s.newValidationContext(ctx, shoot, s.decoder)
	if err != nil {
		return err
	}

	if err := s.validateContext(validationContext).ToAggregate(); err != nil {
		return err
	}

	return s.validateShootSecret(ctx, shoot)
}

func (s *shoot) validateUpdate(ctx context.Context, oldShoot, shoot *core.Shoot) error {
	oldValContext, err := s.newValidationContext(ctx, oldShoot, s.lenientDecoder)
	if err != nil {
		return err
	}

	currentValContext, err := s.newValidationContext(ctx, shoot, s.decoder)
	if err != nil {
		return err
	}

	var (
		oldInfrastructureConfig, currentInfrastructureConfig = oldValContext.infrastructureConfig, currentValContext.infrastructureConfig
		oldControlPlaneConfig, currentControlPlaneConfig     = oldValContext.controlPlaneConfig, currentValContext.controlPlaneConfig
		allErrors                                            = field.ErrorList{}
	)

	if !reflect.DeepEqual(oldInfrastructureConfig, currentInfrastructureConfig) {
		allErrors = append(allErrors, validation.ValidateInfrastructureConfigUpdate(oldInfrastructureConfig, currentInfrastructureConfig, infrastructureConfigPath)...)
	}

	if !reflect.DeepEqual(oldControlPlaneConfig, currentControlPlaneConfig) {
		allErrors = append(allErrors, validation.ValidateControlPlaneConfigUpdate(oldControlPlaneConfig, currentControlPlaneConfig, controlPlaneConfigPath)...)
	}

	allErrors = append(allErrors, validation.ValidateWorkersUpdate(oldValContext.shoot.Spec.Provider.Workers, currentValContext.shoot.Spec.Provider.Workers, workersPath)...)

	for i, currentContext := range currentValContext.workerConfigsContexts {
		currentWorkerConfig := currentContext.workerConfig
		for j, oldContext := range oldValContext.workerConfigsContexts {
			oldWorkerConfig := oldContext.workerConfig
			if shoot.Spec.Provider.Workers[i].Name == oldShoot.Spec.Provider.Workers[j].Name && !reflect.DeepEqual(oldWorkerConfig, currentWorkerConfig) {
				allErrors = append(allErrors, validation.ValidateWorkerConfigUpdate(currentWorkerConfig, oldWorkerConfig, workerConfigPath(i))...)
			}
		}
	}

	allErrors = append(allErrors, s.validateContext(currentValContext)...)

	return allErrors.ToAggregate()

}

func (s *shoot) newValidationContext(ctx context.Context, shoot *core.Shoot, decoder runtime.Decoder) (*validationContext, error) {
	infrastructureConfig := &apiskubevirt.InfrastructureConfig{}
	if shoot.Spec.Provider.InfrastructureConfig != nil {
		var err error
		infrastructureConfig, err = admission.DecodeInfrastructureConfig(decoder, shoot.Spec.Provider.InfrastructureConfig)
		if err != nil {
			return nil, fmt.Errorf("could not decode infrastructureConfig of shoot %q %w", shoot.Name, err)
		}
	}

	controlPlaneConfig := &apiskubevirt.ControlPlaneConfig{}
	if shoot.Spec.Provider.ControlPlaneConfig != nil {
		var err error
		controlPlaneConfig, err = admission.DecodeControlPlaneConfig(decoder, shoot.Spec.Provider.ControlPlaneConfig)
		if err != nil {
			return nil, fmt.Errorf("could not decode controlPlaneConfig of shoot %q %w", shoot.Name, err)
		}
	}

	var workerConfigsContexts []*workerConfigContext
	for _, worker := range shoot.Spec.Provider.Workers {
		workerConfig := &apiskubevirt.WorkerConfig{}
		if worker.ProviderConfig != nil {
			var err error
			workerConfig, err = admission.DecodeWorkerConfig(decoder, worker.ProviderConfig)
			if err != nil {
				return nil, fmt.Errorf("could not decode providerConfig of worker %q %w", worker.Name, err)
			}
		}

		workerConfigsContexts = append(workerConfigsContexts, &workerConfigContext{
			workerConfig: workerConfig,
			dataVolumes:  worker.DataVolumes,
		})
	}

	cloudProfile := &gardencorev1beta1.CloudProfile{}
	if err := s.client.Get(ctx, kutil.Key(shoot.Spec.CloudProfileName), cloudProfile); err != nil {
		return nil, fmt.Errorf("could not get cloud profile %q %w", cloudProfile.Name, err)
	}

	if cloudProfile.Spec.ProviderConfig == nil {
		return nil, fmt.Errorf("missing providerConfig in cloud profile %q", cloudProfile.Name)
	}
	cloudProfileConfig, err := admission.DecodeCloudProfileConfig(decoder, cloudProfile.Spec.ProviderConfig)
	if err != nil {
		return nil, fmt.Errorf("could not decode providerConfig of cloud profile %q %w", cloudProfile.Name, err)
	}

	return &validationContext{
		shoot:                 shoot,
		infrastructureConfig:  infrastructureConfig,
		controlPlaneConfig:    controlPlaneConfig,
		workerConfigsContexts: workerConfigsContexts,
		cloudProfile:          cloudProfile,
		cloudProfileConfig:    cloudProfileConfig,
	}, nil
}

func (s *shoot) validateShootSecret(ctx context.Context, shoot *core.Shoot) error {
	var (
		secretBinding    = &gardencorev1beta1.SecretBinding{}
		secretBindingKey = kutil.Key(shoot.Namespace, shoot.Spec.SecretBindingName)
	)
	if err := kutil.LookupObject(ctx, s.client, s.apiReader, secretBindingKey, secretBinding); err != nil {
		return fmt.Errorf("could not find secret binding %q %w", secretBindingKey.String(), err)
	}

	var (
		secret    = &corev1.Secret{}
		secretKey = kutil.Key(secretBinding.SecretRef.Namespace, secretBinding.SecretRef.Name)
	)
	if err := kutil.LookupObject(ctx, s.client, s.apiReader, secretKey, secret); err != nil {
		return fmt.Errorf("could not find secret %q %w", secretKey.String(), err)
	}

	return validation.ValidateCloudProviderSecret(secret)
}
