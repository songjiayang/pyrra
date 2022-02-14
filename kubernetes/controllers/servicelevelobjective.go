/*
Copyright 2021 Pyrra Authors.

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

package controllers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	pyrrav1alpha1 "github.com/pyrra-dev/pyrra/kubernetes/api/v1alpha1"
)

// ServiceLevelObjectiveReconciler reconciles a ServiceLevelObjective object
type ServiceLevelObjectiveReconciler struct {
	client.Client
	Log           logr.Logger
	Scheme        *runtime.Scheme
	ConfigMapMode bool
}

// +kubebuilder:rbac:groups=pyrra.dev,resources=servicelevelobjectives,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pyrra.dev,resources=servicelevelobjectives/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules/status,verbs=get

func (r *ServiceLevelObjectiveReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("servicelevelobjective", req.NamespacedName)
	log.Info("reconciling")

	var slo pyrrav1alpha1.ServiceLevelObjective
	if err := r.Get(ctx, req.NamespacedName, &slo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if r.ConfigMapMode {
		return r.reconcileConfigMap(ctx, log, req, slo)
	}

	return r.reconcilePrometheusRule(ctx, log, req, slo)
}

func (r *ServiceLevelObjectiveReconciler) reconcilePrometheusRule(ctx context.Context, logger logr.Logger, req ctrl.Request, kubeObjective pyrrav1alpha1.ServiceLevelObjective) (ctrl.Result, error) {
	newRule, err := makePrometheusRule(kubeObjective)
	if err != nil {
		return ctrl.Result{}, err
	}

	var rule monitoringv1.PrometheusRule
	if err := r.Get(ctx, req.NamespacedName, &rule); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("creating prometheus rule", "name", rule.GetName(), "namespace", rule.GetNamespace())
			if err := r.Create(ctx, newRule); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	newRule.ResourceVersion = rule.ResourceVersion

	logger.Info("updating prometheus rule", "name", rule.GetName(), "namespace", rule.GetNamespace())
	if err := r.Update(ctx, newRule); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ServiceLevelObjectiveReconciler) reconcileConfigMap(ctx context.Context, logger logr.Logger, req ctrl.Request, kubeObjective pyrrav1alpha1.ServiceLevelObjective) (ctrl.Result, error) {
	name := fmt.Sprintf("pyrra-recording-rule-%s", kubeObjective.GetName())

	newConfigMap, err := makeConfigMap(name, kubeObjective)
	if err != nil {
		return ctrl.Result{}, err
	}

	var existingConfigMap corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: req.Namespace,
		Name:      name,
	}, &existingConfigMap); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("creating config map", "name", newConfigMap.GetName(), "namespace", newConfigMap.GetNamespace())
			if err := r.Create(ctx, newConfigMap); err != nil {
				return ctrl.Result{}, fmt.Errorf("creating config map: %w", err)
			}
		}

		return ctrl.Result{}, err
	}

	newConfigMap.ResourceVersion = existingConfigMap.ResourceVersion

	logger.Info("updating config map", "name", newConfigMap.ObjectMeta.GetName(), "namespace", newConfigMap.ObjectMeta.GetNamespace())
	if err := r.Update(ctx, newConfigMap); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating config map: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *ServiceLevelObjectiveReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pyrrav1alpha1.ServiceLevelObjective{}).
		Complete(r)
}

func makeConfigMap(name string, kubeObjective pyrrav1alpha1.ServiceLevelObjective) (*corev1.ConfigMap, error) {
	slo, err := kubeObjective.Internal()
	if err != nil {
		return nil, fmt.Errorf("getting SLO: %w", err)
	}

	ruleGroup, err := slo.Burnrates()
	if err != nil {
		return nil, fmt.Errorf("getting recording rules from SLO: %w", err)
	}

	rule := monitoringv1.PrometheusRuleSpec{
		Groups: []monitoringv1.RuleGroup{ruleGroup},
	}

	bytes, err := yaml.Marshal(rule)
	if err != nil {
		return nil, fmt.Errorf("marshalling recording rule: %w", err)
	}

	data := map[string]string{
		fmt.Sprintf("%s.rules.yaml", name): string(bytes),
	}

	isController := true
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: kubeObjective.GetNamespace(),
			Labels:    kubeObjective.GetLabels(),
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: kubeObjective.APIVersion,
					Kind:       kubeObjective.Kind,
					Name:       kubeObjective.Name,
					UID:        kubeObjective.UID,
					Controller: &isController,
				},
			},
		},
		Data: data,
	}, nil
}

func makePrometheusRule(kubeObjective pyrrav1alpha1.ServiceLevelObjective) (*monitoringv1.PrometheusRule, error) {
	objective, err := kubeObjective.Internal()
	if err != nil {
		return nil, err
	}

	group, err := objective.Burnrates()
	if err != nil {
		return nil, err
	}

	isController := true
	return &monitoringv1.PrometheusRule{
		TypeMeta: metav1.TypeMeta{
			Kind:       monitoringv1.PrometheusRuleKind,
			APIVersion: monitoring.GroupName + "/" + monitoringv1.Version,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeObjective.GetName(),
			Namespace: kubeObjective.GetNamespace(),
			Labels:    kubeObjective.GetLabels(),
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: kubeObjective.APIVersion,
					Kind:       kubeObjective.Kind,
					Name:       kubeObjective.Name,
					UID:        kubeObjective.UID,
					Controller: &isController,
				},
			},
		},
		Spec: monitoringv1.PrometheusRuleSpec{
			Groups: []monitoringv1.RuleGroup{group},
		},
	}, nil
}
