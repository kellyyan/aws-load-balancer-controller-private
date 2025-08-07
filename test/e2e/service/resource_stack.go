package service

import (
	"context"
	"fmt"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/algorithm"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/k8s"
	"sigs.k8s.io/aws-load-balancer-controller/test/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strconv"
)

func NewResourceStack(dp *appsv1.Deployment, svcs []*corev1.Service, baseName string, enablePodReadinessGate bool) *resourceStack {
	return &resourceStack{
		dp:                     dp,
		svcs:                   svcs,
		baseName:               baseName,
		enablePodReadinessGate: enablePodReadinessGate,
	}
}

// resourceStack containing the deployment and service resources
type resourceStack struct {
	// configurations
	svcs                   []*corev1.Service // When creating multiple services, ensure first service is of type load balancer
	dp                     *appsv1.Deployment
	ns                     *corev1.Namespace
	baseName               string
	enablePodReadinessGate bool

	// runtime variables
	createdDP  *appsv1.Deployment
	createdSVC *corev1.Service
}

func (s *resourceStack) Deploy(ctx context.Context, f *framework.Framework) error {
	if err := s.allocateNamespace(ctx, f); err != nil {
		return err
	}
	s.dp.Namespace = s.ns.Name
	for _, svc := range s.svcs {
		svc.Namespace = s.ns.Name
	}
	if err := s.createDeployment(ctx, f); err != nil {
		return err
	}
	if err := s.createServices(ctx, f); err != nil {
		return err
	}
	if err := s.waitUntilDeploymentReady(ctx, f); err != nil {
		return err
	}
	if err := s.waitUntilServiceReady(ctx, f); err != nil {
		return err
	}
	return nil
}

func (s *resourceStack) UpdateServiceAnnotations(ctx context.Context, f *framework.Framework, svcAnnotations map[string]string) error {
	if err := s.updateServiceAnnotations(ctx, f, svcAnnotations); err != nil {
		return err
	}
	if err := s.waitUntilServiceReady(ctx, f); err != nil {
		return err
	}
	return nil
}

func (s *resourceStack) DeleteServiceAnnotations(ctx context.Context, f *framework.Framework, annotationKeys []string) error {
	if err := s.removeServiceAnnotations(ctx, f, annotationKeys); err != nil {
		return err
	}
	if err := s.waitUntilServiceReady(ctx, f); err != nil {
		return err
	}
	return nil
}

func (s *resourceStack) UpdateServiceTrafficPolicy(ctx context.Context, f *framework.Framework, trafficPolicy corev1.ServiceExternalTrafficPolicyType) error {
	if err := s.updateServiceTrafficPolicy(ctx, f, trafficPolicy); err != nil {
		return err
	}
	if err := s.waitUntilServiceReady(ctx, f); err != nil {
		return err
	}
	return nil
}

func (s *resourceStack) ScaleDeployment(ctx context.Context, f *framework.Framework, numReplicas int32) error {
	f.Logger.Info("scaling deployment", "dp", k8s.NamespacedName(s.dp), "currentReplicas", s.dp.Spec.Replicas, "desiredReplicas", numReplicas)
	oldDP := s.dp.DeepCopy()
	s.dp.Spec.Replicas = &numReplicas
	if err := f.K8sClient.Patch(ctx, s.dp, client.MergeFrom(oldDP)); err != nil {
		f.Logger.Info("failed to update deployment", "dp", k8s.NamespacedName(s.dp))
		return err
	}
	if err := s.waitUntilDeploymentReady(ctx, f); err != nil {
		return err
	}
	return nil
}

func (s *resourceStack) Cleanup(ctx context.Context, f *framework.Framework) error {
	if err := s.deleteNamespace(ctx, f); err != nil {
		return err
	}
	return nil
}

func (s *resourceStack) GetLoadBalancerIngressHostname() string {
	return s.createdSVC.Status.LoadBalancer.Ingress[0].Hostname
}

func (s *resourceStack) GetStackName() string {
	return fmt.Sprintf("%v/%v", s.ns.Name, s.svcs[0].Name)
}

func (s *resourceStack) getListenersPortMap() map[string]string {
	listenersMap := map[string]string{}
	for _, port := range s.createdSVC.Spec.Ports {
		listenersMap[strconv.Itoa(int(port.Port))] = string(port.Protocol)
	}
	return listenersMap
}

func (s *resourceStack) getTargetGroupNodePortMap() map[string][]string {
	tgPortProtocolMap := map[string][]string{}
	for _, port := range s.createdSVC.Spec.Ports {
		tgPortProtocolMap[strconv.Itoa(int(port.NodePort))] = []string{string(port.Protocol)}
	}
	return tgPortProtocolMap
}

func (s *resourceStack) getHealthCheckNodePort() string {
	return strconv.Itoa(int(s.svcs[0].Spec.HealthCheckNodePort))
}

func (s *resourceStack) updateServiceTrafficPolicy(ctx context.Context, f *framework.Framework, trafficPolicy corev1.ServiceExternalTrafficPolicyType) error {
	f.Logger.Info("updating service annotations", "svc", k8s.NamespacedName(s.svcs[0]))
	oldSvc := s.svcs[0].DeepCopy()
	s.svcs[0].Spec.ExternalTrafficPolicy = trafficPolicy
	return s.updateService(ctx, f, oldSvc)
}

func (s *resourceStack) updateServiceAnnotations(ctx context.Context, f *framework.Framework, svcAnnotations map[string]string) error {
	f.Logger.Info("updating service annotations", "svc", k8s.NamespacedName(s.svcs[0]))
	oldSvc := s.svcs[0].DeepCopy()
	for key, value := range svcAnnotations {
		s.svcs[0].Annotations[key] = value
	}
	return s.updateService(ctx, f, oldSvc)
}

func (s *resourceStack) removeServiceAnnotations(ctx context.Context, f *framework.Framework, annotationKeys []string) error {
	f.Logger.Info("removing service annotations", "svc", k8s.NamespacedName(s.svcs[0]))
	oldSvc := s.svcs[0].DeepCopy()
	for _, key := range annotationKeys {
		delete(s.svcs[0].Annotations, key)
	}
	return s.updateService(ctx, f, oldSvc)
}

func (s *resourceStack) updateService(ctx context.Context, f *framework.Framework, oldSvc *corev1.Service) error {
	f.Logger.Info("updating service", "svc", k8s.NamespacedName(s.svcs[0]))
	if err := f.K8sClient.Patch(ctx, s.svcs[0], client.MergeFrom(oldSvc)); err != nil {
		f.Logger.Info("failed to update service", "svc", k8s.NamespacedName(s.svcs[0]))
		return err
	}
	return nil
}

func (s *resourceStack) createDeployment(ctx context.Context, f *framework.Framework) error {
	f.Logger.Info("creating deployment", "dp", k8s.NamespacedName(s.dp))
	if err := f.K8sClient.Create(ctx, s.dp); err != nil {
		f.Logger.Info("failed to create deployment")
		return err
	}
	f.Logger.Info("created deployment", "dp", k8s.NamespacedName(s.dp))
	return nil
}

func (s *resourceStack) waitUntilDeploymentReady(ctx context.Context, f *framework.Framework) error {
	f.Logger.Info("waiting until deployment becomes ready", "dp", k8s.NamespacedName(s.dp))
	observedDP, err := f.DPManager.WaitUntilDeploymentReady(ctx, s.dp)
	if err != nil {
		f.Logger.Info("failed waiting for deployment")
		return err
	}
	f.Logger.Info("deployment is ready", "dp", k8s.NamespacedName(s.dp))
	s.createdDP = observedDP
	return nil
}

func (s *resourceStack) createServices(ctx context.Context, f *framework.Framework) error {
	f.Logger.Info("create all services")

	for _, svc := range s.svcs {
		f.Logger.Info("creating service", "svc", k8s.NamespacedName(svc))
		svc = svc.DeepCopy()
		if err := f.K8sClient.Create(ctx, svc); err != nil {
			return err
		}
		if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
			s.createdSVC = svc
		}
		f.Logger.Info("created service", "svc", k8s.NamespacedName(svc))
	}

	return nil
}

func (s *resourceStack) waitUntilServiceReady(ctx context.Context, f *framework.Framework) error {
	f.Logger.Info("waiting for service of type load balancer to become ready", "svc", k8s.NamespacedName(s.createdSVC))
	observedSVC, err := f.SVCManager.WaitUntilServiceActive(ctx, s.createdSVC)
	if err != nil {
		return err
	}
	s.createdSVC = observedSVC
	return nil
}

func (s *resourceStack) allocateNamespace(ctx context.Context, f *framework.Framework) error {
	f.Logger.Info("allocating namespace")
	ns, err := f.NSManager.AllocateNamespace(ctx, s.baseName)
	if err != nil {
		return err
	}
	s.ns = ns
	f.Logger.Info("allocated namespace", "nsName", s.ns.Name)
	if s.enablePodReadinessGate {
		f.Logger.Info("label namespace for podReadinessGate injection", "nsName", s.ns.Name)
		oldNS := s.ns.DeepCopy()
		s.ns.Labels = algorithm.MergeStringMap(map[string]string{
			"elbv2.k8s.aws/pod-readiness-gate-inject": "enabled",
		}, s.ns.Labels)
		err := f.K8sClient.Patch(ctx, ns, client.MergeFrom(oldNS))
		if err != nil {
			return err
		}
		f.Logger.Info("labeled namespace with podReadinessGate injection", "nsName", s.ns.Name)
	}
	return nil
}

func (s *resourceStack) deleteNamespace(ctx context.Context, tf *framework.Framework) error {
	tf.Logger.Info("deleting namespace", "ns", k8s.NamespacedName(s.ns))
	if err := tf.K8sClient.Delete(ctx, s.ns); err != nil {
		tf.Logger.Info("failed to delete namespace", "ns", k8s.NamespacedName(s.ns))
		return err
	}
	if err := tf.NSManager.WaitUntilNamespaceDeleted(ctx, s.ns); err != nil {
		tf.Logger.Info("failed to wait for namespace deletion", "ns", k8s.NamespacedName(s.ns))
		return err
	}
	tf.Logger.Info("deleted namespace", "ns", k8s.NamespacedName(s.ns))
	return nil
}
