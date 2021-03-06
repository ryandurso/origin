package origin

import (
	"fmt"
	"io/ioutil"
	"time"

	"github.com/golang/glog"

	kapierror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/retry"

	"github.com/openshift/origin/pkg/cmd/admin/policy"

	authorizationapi "github.com/openshift/origin/pkg/authorization/api"
	clusterpolicyregistry "github.com/openshift/origin/pkg/authorization/registry/clusterpolicy"
	clusterpolicystorage "github.com/openshift/origin/pkg/authorization/registry/clusterpolicy/etcd"
	"github.com/openshift/origin/pkg/cmd/server/admin"
	"github.com/openshift/origin/pkg/cmd/server/bootstrappolicy"
)

// ensureOpenShiftSharedResourcesNamespace is called as part of global policy initialization to ensure shared namespace exists
func (c *MasterConfig) ensureOpenShiftSharedResourcesNamespace() {
	if _, err := c.KubeClientsetInternal().Core().Namespaces().Get(c.Options.PolicyConfig.OpenShiftSharedResourcesNamespace, metav1.GetOptions{}); kapierror.IsNotFound(err) {
		namespace, createErr := c.KubeClientsetInternal().Core().Namespaces().Create(&kapi.Namespace{ObjectMeta: metav1.ObjectMeta{Name: c.Options.PolicyConfig.OpenShiftSharedResourcesNamespace}})
		if createErr != nil {
			glog.Errorf("Error creating namespace: %v due to %v\n", c.Options.PolicyConfig.OpenShiftSharedResourcesNamespace, createErr)
			return
		}

		c.ensureNamespaceServiceAccountRoleBindings(namespace)
	}
}

// ensureOpenShiftInfraNamespace is called as part of global policy initialization to ensure infra namespace exists
func (c *MasterConfig) ensureOpenShiftInfraNamespace() {
	ns := c.Options.PolicyConfig.OpenShiftInfrastructureNamespace

	// Ensure namespace exists
	namespace, err := c.KubeClientsetInternal().Core().Namespaces().Create(&kapi.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	if kapierror.IsAlreadyExists(err) {
		// Get the persisted namespace
		namespace, err = c.KubeClientsetInternal().Core().Namespaces().Get(ns, metav1.GetOptions{})
		if err != nil {
			glog.Errorf("Error getting namespace %s: %v", ns, err)
			return
		}
	} else if err != nil {
		glog.Errorf("Error creating namespace %s: %v", ns, err)
		return
	}

	roleAccessor := policy.NewClusterRoleBindingAccessor(c.ServiceAccountRoleBindingClient())
	for _, saName := range bootstrappolicy.InfraSAs.GetServiceAccounts() {
		_, err := c.KubeClientsetInternal().Core().ServiceAccounts(ns).Create(&kapi.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName}})
		if err != nil && !kapierror.IsAlreadyExists(err) {
			glog.Errorf("Error creating service account %s/%s: %v", ns, saName, err)
		}

		role, _ := bootstrappolicy.InfraSAs.RoleFor(saName)

		reconcileRole := &policy.ReconcileClusterRolesOptions{
			RolesToReconcile: []string{role.Name},
			Confirmed:        true,
			Union:            true,
			Out:              ioutil.Discard,
			RoleClient:       c.PrivilegedLoopbackOpenShiftClient.ClusterRoles(),
		}
		if err := reconcileRole.RunReconcileClusterRoles(nil, nil); err != nil {
			glog.Errorf("Could not reconcile %v: %v\n", role.Name, err)
		}

		addRole := &policy.RoleModificationOptions{
			RoleName:            role.Name,
			RoleBindingAccessor: roleAccessor,
			Subjects:            []kapi.ObjectReference{{Namespace: ns, Name: saName, Kind: "ServiceAccount"}},
		}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error { return addRole.AddRole() }); err != nil {
			glog.Errorf("Could not add %v service accounts to the %v cluster role: %v\n", saName, role.Name, err)
		} else {
			glog.V(2).Infof("Added %v service accounts to the %v cluster role: %v\n", saName, role.Name, err)
		}
	}

	c.ensureNamespaceServiceAccountRoleBindings(namespace)
}

// ensureDefaultNamespaceServiceAccountRoles initializes roles for service accounts in the default namespace
func (c *MasterConfig) ensureDefaultNamespaceServiceAccountRoles() {
	// Wait for the default namespace
	var namespace *kapi.Namespace
	for i := 0; i < 30; i++ {
		ns, err := c.KubeClientsetInternal().Core().Namespaces().Get(metav1.NamespaceDefault, metav1.GetOptions{})
		if err == nil {
			namespace = ns
			break
		}
		if kapierror.IsNotFound(err) {
			time.Sleep(time.Second)
			continue
		}
		glog.Errorf("Error adding service account roles to %q namespace: %v", metav1.NamespaceDefault, err)
		return
	}
	if namespace == nil {
		glog.Errorf("Namespace %q not found, could not initialize the %q namespace", metav1.NamespaceDefault, metav1.NamespaceDefault)
		return
	}

	c.ensureNamespaceServiceAccountRoleBindings(namespace)
}

// ensureNamespaceServiceAccountRoleBindings initializes roles for service accounts in the namespace
func (c *MasterConfig) ensureNamespaceServiceAccountRoleBindings(namespace *kapi.Namespace) {
	const ServiceAccountRolesInitializedAnnotation = "openshift.io/sa.initialized-roles"

	// Short-circuit if we're already initialized
	if namespace.Annotations[ServiceAccountRolesInitializedAnnotation] == "true" {
		return
	}

	hasErrors := false
	for _, binding := range bootstrappolicy.GetBootstrapServiceAccountProjectRoleBindings(namespace.Name) {
		addRole := &policy.RoleModificationOptions{
			RoleName:            binding.RoleRef.Name,
			RoleNamespace:       binding.RoleRef.Namespace,
			RoleBindingAccessor: policy.NewLocalRoleBindingAccessor(namespace.Name, c.ServiceAccountRoleBindingClient()),
			Subjects:            binding.Subjects,
		}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error { return addRole.AddRole() }); err != nil {
			glog.Errorf("Could not add service accounts to the %v role in the %q namespace: %v\n", binding.RoleRef.Name, namespace.Name, err)
			hasErrors = true
		}
	}

	// If we had errors, don't register initialization so we can try again
	if hasErrors {
		return
	}

	if namespace.Annotations == nil {
		namespace.Annotations = map[string]string{}
	}
	namespace.Annotations[ServiceAccountRolesInitializedAnnotation] = "true"
	// Log any error other than a conflict (the update will be retried and recorded again on next startup in that case)
	if _, err := c.KubeClientsetInternal().Core().Namespaces().Update(namespace); err != nil && !kapierror.IsConflict(err) {
		glog.Errorf("Error recording adding service account roles to %q namespace: %v", namespace.Name, err)
	}
}

func (c *MasterConfig) securityContextConstraintsSupported() (bool, error) {
	// TODO to make this a library upstream, ResourceExists(GroupVersionResource) or some such.
	// look for supported groups
	serverGroupList, err := c.KubeClientsetInternal().Discovery().ServerGroups()
	if err != nil {
		return false, err
	}
	// find the preferred version of the legacy group
	var legacyGroup *metav1.APIGroup
	for i := range serverGroupList.Groups {
		if len(serverGroupList.Groups[i].Name) == 0 {
			legacyGroup = &serverGroupList.Groups[i]
		}
	}
	if legacyGroup == nil {
		return false, fmt.Errorf("unable to discovery preferred version for legacy api group")
	}
	// check if securitycontextconstraints is a resource in the group
	apiResourceList, err := c.KubeClientsetInternal().Discovery().ServerResourcesForGroupVersion(legacyGroup.PreferredVersion.GroupVersion)
	if err != nil {
		return false, err
	}
	for _, apiResource := range apiResourceList.APIResources {
		if apiResource.Name == "securitycontextconstraints" && !apiResource.Namespaced {
			return true, nil
		}
	}
	return false, nil
}

func (c *MasterConfig) ensureDefaultSecurityContextConstraints() {
	sccSupported, err := c.securityContextConstraintsSupported()
	if err != nil {
		glog.Errorf("Unable to determine if security context constraints are supported. Got error: %v", err)
		return
	}
	if !sccSupported {
		glog.Infof("Ignoring default security context constraints when running on external Kubernetes.")
		return
	}

	ns := c.Options.PolicyConfig.OpenShiftInfrastructureNamespace
	bootstrapSCCGroups, bootstrapSCCUsers := bootstrappolicy.GetBoostrapSCCAccess(ns)

	for _, scc := range bootstrappolicy.GetBootstrapSecurityContextConstraints(bootstrapSCCGroups, bootstrapSCCUsers) {
		_, err := c.KubeClientsetInternal().Core().SecurityContextConstraints().Create(&scc)
		if kapierror.IsAlreadyExists(err) {
			continue
		}
		if err != nil {
			glog.Errorf("Unable to create default security context constraint %s.  Got error: %v", scc.Name, err)
			continue
		}
		glog.Infof("Created default security context constraint %s", scc.Name)
	}
}

// ensureComponentAuthorizationRules initializes the cluster policies
func (c *MasterConfig) ensureComponentAuthorizationRules() {
	clusterPolicyStorage, err := clusterpolicystorage.NewREST(c.RESTOptionsGetter)
	if err != nil {
		glog.Errorf("Error creating policy storage: %v", err)
		return
	}
	clusterPolicyRegistry := clusterpolicyregistry.NewRegistry(clusterPolicyStorage)
	ctx := apirequest.WithNamespace(apirequest.NewContext(), "")

	if _, err := clusterPolicyRegistry.GetClusterPolicy(ctx, authorizationapi.PolicyName, &metav1.GetOptions{}); kapierror.IsNotFound(err) {
		glog.Infof("No cluster policy found.  Creating bootstrap policy based on: %v", c.Options.PolicyConfig.BootstrapPolicyFile)

		if err := admin.OverwriteBootstrapPolicy(c.RESTOptionsGetter, c.Options.PolicyConfig.BootstrapPolicyFile, admin.CreateBootstrapPolicyFileFullCommand, true, ioutil.Discard); err != nil {
			glog.Errorf("Error creating bootstrap policy: %v", err)
		}

	} else {
		glog.V(2).Infof("Ignoring bootstrap policy file because cluster policy found")
	}

	// Wait until the policy cache has caught up before continuing
	review := &authorizationapi.SubjectAccessReview{Action: authorizationapi.Action{Verb: "get", Group: authorizationapi.GroupName, Resource: "clusterpolicies"}}
	err = wait.PollImmediate(100*time.Millisecond, 30*time.Second, func() (done bool, err error) {
		result, err := c.PolicyClient().SubjectAccessReviews().Create(review)
		if err == nil && result.Allowed {
			return true, nil
		}
		if kapierror.IsForbidden(err) || (err == nil && !result.Allowed) {
			glog.V(2).Infof("waiting for policy cache to initialize")
			return false, nil
		}
		return false, err
	})
	if err != nil {
		glog.Errorf("error waiting for policy cache to initialize: %v", err)
	}

	// Reconcile roles that must exist for the cluster to function
	// Be very judicious about what is placed in this list, since it will be enforced on every server start
	reconcileRoles := &policy.ReconcileClusterRolesOptions{
		RolesToReconcile: []string{bootstrappolicy.DiscoveryRoleName},
		Confirmed:        true,
		Union:            true,
		Out:              ioutil.Discard,
		RoleClient:       c.PrivilegedLoopbackOpenShiftClient.ClusterRoles(),
	}
	if err := reconcileRoles.RunReconcileClusterRoles(nil, nil); err != nil {
		glog.Errorf("Could not auto reconcile roles: %v\n", err)
	}

	// Reconcile rolebindings that must exist for the cluster to function
	// Be very judicious about what is placed in this list, since it will be enforced on every server start
	reconcileRoleBindings := &policy.ReconcileClusterRoleBindingsOptions{
		RolesToReconcile:  []string{bootstrappolicy.DiscoveryRoleName},
		Confirmed:         true,
		Union:             true,
		Out:               ioutil.Discard,
		RoleBindingClient: c.PrivilegedLoopbackOpenShiftClient.ClusterRoleBindings(),
	}
	if err := reconcileRoleBindings.RunReconcileClusterRoleBindings(nil, nil); err != nil {
		glog.Errorf("Could not auto reconcile role bindings: %v\n", err)
	}
}
