package application_event_loop

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/go-logr/logr"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/db"
	dbutil "github.com/redhat-appstudio/managed-gitops/backend-shared/db/util"

	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	argosharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util/argocd"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/fauxargocd"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/gitopserrors"
	logutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util/log"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/operations"
	"github.com/redhat-appstudio/managed-gitops/backend/condition"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/eventlooptypes"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/shared_resource_loop"
	"github.com/redhat-appstudio/managed-gitops/backend/metrics"
	goyaml "gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type deploymentModifiedResult string

const (
	deploymentModifiedResult_Failed   deploymentModifiedResult = "failed"
	deploymentModifiedResult_Deleted  deploymentModifiedResult = "deletedApp"
	deploymentModifiedResult_Created  deploymentModifiedResult = "createdNewApp"
	deploymentModifiedResult_Updated  deploymentModifiedResult = "updatedApp"
	deploymentModifiedResult_NoChange deploymentModifiedResult = "noChangeInApp"

	prunePropagationPolicy = "PrunePropagationPolicy=background"
	appProjectPrefix       = "app-project-"
)

// This file is responsible for processing events related to GitOpsDeployment CR.

// applicationEventRunner_handleDeploymentModified handles GitOpsDeployment resource events, ensuring that the
// database is consistent with the corresponding GitOpsDeployment resource.
//
// For example:
// - If a GitOpsDeployment resource is created in the namespace, ensure there exists a corresponding Application database row.
// - If a GitOpsDeployment resource is modified in the namespace, ensure the corresponding Application database row is modified.
// - Likewise, if a GitOpsDeployment previously existed, and has been deleted, then ensure the Application is cleaned up.
//
// If a change is made to the database, the cluster-agent will be informed by creating an Operation resource, pointing to the changed Application.
//
// Returns:
// - true if the goroutine responsible for this application can shutdown (e.g. because the GitOpsDeployment no longer exists, so no longer needs to be processed), false otherwise.
// - references to the Application and GitOpsEngineInstance database fields.
// - error is non-nil, if an error occurred
func (a *applicationEventLoopRunner_Action) applicationEventRunner_handleDeploymentModified(ctx context.Context,
	dbQueries db.ApplicationScopedQueries) (bool, *db.Application, *db.GitopsEngineInstance, deploymentModifiedResult,
	gitopserrors.UserError) {

	const (
		signalledShutdown_true  = true
		signalledShutdown_false = false
	)

	deplName := a.eventResourceName
	deplNamespace := a.eventResourceNamespace

	gitopsDeplNamespace := corev1.Namespace{}

	if err := a.workspaceClient.Get(ctx, types.NamespacedName{Name: deplNamespace}, &gitopsDeplNamespace); err != nil {
		userError := fmt.Sprintf("unable to retrieve the contents of the namespace '%s' containing the API resource '%s'. Does it exist?",
			deplNamespace, deplName)
		devError := fmt.Errorf("unable to retrieve namespace '%s': %v", deplNamespace, err)
		return signalledShutdown_false, nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewUserDevError(userError, devError)
	}

	clusterUser, _, err := a.sharedResourceEventLoop.GetOrCreateClusterUserByNamespaceUID(ctx, a.workspaceClient, gitopsDeplNamespace, a.log)
	if err != nil {
		userError := "unable to locate managed environment for new application"
		devError := fmt.Errorf("unable to retrieve cluster user in handleDeploymentModified, '%s': %v",
			string(gitopsDeplNamespace.UID), err)
		return signalledShutdown_false, nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewUserDevError(userError, devError)
	}

	// 1) Retrieve the GitOpsDeployment from the namespace
	gitopsDeployment := &managedgitopsv1alpha1.GitOpsDeployment{}
	{
		gitopsDeploymentKey := client.ObjectKey{Namespace: deplNamespace, Name: deplName}

		if err := a.workspaceClient.Get(ctx, gitopsDeploymentKey, gitopsDeployment); err != nil {

			if apierr.IsNotFound(err) {
				// Not found, so set gitopsDeployment to nil
				gitopsDeployment = nil
			} else {
				userError := "unable to retrieve the GitOpsDeployment object from the namespace, due to unknown error."
				a.log.Error(err, "unable to locate object in handleDeploymentModified", "request", gitopsDeploymentKey)
				return signalledShutdown_false, nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewUserDevError(userError, err)
			}
		}
	}

	if !isGitOpsDeploymentDeleted(gitopsDeployment) {
		// Perform basic validation of GitOpsDeployment values

		if gitopsDeployment.Spec.Source.Path == "" {
			userError := managedgitopsv1alpha1.GitOpsDeploymentUserError_PathIsRequired
			return signalledShutdown_false, nil, nil, deploymentModifiedResult_Failed,
				gitopserrors.NewUserDevError(userError, fmt.Errorf(userError))

		} else if gitopsDeployment.Spec.Source.Path == "/" {
			userError := managedgitopsv1alpha1.GitOpsDeploymentUserError_InvalidPathSlash
			return signalledShutdown_false, nil, nil, deploymentModifiedResult_Failed,
				gitopserrors.NewUserDevError(userError, fmt.Errorf(userError))

		}
	}

	// Update the list of GitOpsDeployments that we use to generate metrics
	if !isGitOpsDeploymentDeleted(gitopsDeployment) {
		metrics.AddOrUpdateGitOpsDeployment(deplName, deplNamespace, string(gitopsDeplNamespace.UID))
	} else {
		metrics.RemoveGitOpsDeployment(deplName, deplNamespace, string(gitopsDeplNamespace.UID))
	}

	// 2) Look for any DTAMs that point(ed) to a K8s resource with the same name and namespace as this request
	var deplToAppMappingList []db.DeploymentToApplicationMapping
	if err := dbQueries.ListDeploymentToApplicationMappingByNamespaceAndName(ctx, deplName, deplNamespace,
		eventlooptypes.GetWorkspaceIDFromNamespaceID(gitopsDeplNamespace), &deplToAppMappingList); err != nil {

		userError := "unable to retrieve GitOpsDeployment metadata from the database, due to an unknown error"
		a.log.Error(err, "unable to retrieve deployment to application mapping by name/namespace/uid",
			"name", deplName, "namespace", deplNamespace, "UID", string(gitopsDeplNamespace.UID))

		return signalledShutdown_false, nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewUserDevError(userError, err)
	}

	// DTAM that matches the current resource UID (or nil if not found)
	var currentDeplToAppMapping *db.DeploymentToApplicationMapping

	// old DTAMs that don't match the current resource UID (pointing to previously deleted GitOpsDeployments)
	oldDeplToAppMappings := []db.DeploymentToApplicationMapping{}

	// 3) Identify DTAMs that refer to K8s resources that no longer exist, and find the DTAM that
	// refers to the resource we got the request for.
	for idx := range deplToAppMappingList {

		dtam := deplToAppMappingList[idx]

		if !isGitOpsDeploymentDeleted(gitopsDeployment) && dtam.Deploymenttoapplicationmapping_uid_id == string(gitopsDeployment.UID) {
			currentDeplToAppMapping = &dtam
		} else {
			oldDeplToAppMappings = append(oldDeplToAppMappings, dtam)
		}
	}

	// 4) Clean up any old GitOpsDeployments that have the same name/namespace as this resource, but that no longer exist
	successfulCleanup := signalledShutdown_true
	if len(oldDeplToAppMappings) > 0 || isGitOpsDeploymentDeleted(gitopsDeployment) {

		var deleteErr gitopserrors.UserError
		successfulCleanup, deleteErr = a.handleDeleteGitOpsDeplEvent(ctx, gitopsDeployment, clusterUser, &oldDeplToAppMappings, dbQueries)
		if deleteErr != nil {
			return signalledShutdown_false, nil, nil, deploymentModifiedResult_Failed, deleteErr
		}
		//}
	}

	// 5) Finally, handle the resource event, based on whether it is a create, update, or no-op

	if !isGitOpsDeploymentDeleted(gitopsDeployment) {
		// If the GitOpsDeployment resource exists in the namespace

		if currentDeplToAppMapping == nil {
			// 5a) If the gitopsdepl CR exists, but the database entry doesn't,
			// then this is the first time we have seen the GitOpsDepl CR.
			// Create it in the DB and create the operation.
			application, gitopsEngineInstance, deplModifiedResult, err :=
				a.handleNewGitOpsDeplEvent(ctx, *gitopsDeployment, clusterUser, dbQueries)

			// Since the GitOpsDeployment still exists, don't signal shutdown
			return signalledShutdown_false, application, gitopsEngineInstance, deplModifiedResult, err

		} else {

			// 5b) if both exist: it's an update (or a no-op)
			application, gitopsEngineInstance, deplModifiedResult, err := a.handleUpdatedGitOpsDeplEvent(ctx, currentDeplToAppMapping,
				*gitopsDeployment, clusterUser, dbQueries)

			// Since the GitOpsDeployment still exists, don't signal shutdown
			return signalledShutdown_false, application, gitopsEngineInstance, deplModifiedResult, err
		}

	} else {

		// 5c) If the GitOpsDeployment resource doesn't exist, and we already deleted old resources above, then our work is done.
		// We can signal to shutdown the goroutine.
		return successfulCleanup, nil, nil, deploymentModifiedResult_Deleted, nil
	}

}

// handleNewGitOpsDeplEvent handles GitOpsDeployment events where the user has just created a new GitOpsDeployment resource.
// In this case, we need to create Application and DeploymentToApplicationMapping rows in the database (among others).
//
// Finally, we need to inform the cluster-agent component (via Operation), so that it configures Argo CD.
//
// Returns:
//   - true if the goroutine responsible for this application can shutdown (e.g. because the GitOpsDeployment
//     no longer exists, so no longer needs to be processed), false otherwise.
//   - references to the Application and GitOpsEngineInstance database fields.
//   - error is non-nil, if an error occurred
func (a applicationEventLoopRunner_Action) handleNewGitOpsDeplEvent(ctx context.Context,
	gitopsDeployment managedgitopsv1alpha1.GitOpsDeployment, clusterUser *db.ClusterUser,
	dbQueries db.ApplicationScopedQueries) (*db.Application, *db.GitopsEngineInstance, deploymentModifiedResult, gitopserrors.UserError) {

	a.log.Info("Received GitOpsDeployment event for a new GitOpsDeployment resource")
	gitopsDeplNamespace := corev1.Namespace{}
	if err := a.workspaceClient.Get(ctx, types.NamespacedName{
		Name: gitopsDeployment.ObjectMeta.Namespace}, &gitopsDeplNamespace); err != nil {

		userError := "unable to access the Namespace containing the GitOpsDeployment resource"
		devError := fmt.Errorf("unable to retrieve namespace for managed env, '%s': %v", gitopsDeployment.ObjectMeta.Namespace, err)

		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewUserDevError(userError, devError)

	}

	// Don't process a new GitOpsDeployment in a Namespace that is in the process of being deleted.
	if gitopsDeplNamespace.DeletionTimestamp != nil {
		a.log.Info("Skipping GitOpsDeployment event in Namespace that is being deleted")
		return nil, nil, deploymentModifiedResult_NoChange, nil
	}

	isWorkspaceTarget := gitopsDeployment.Spec.Destination.Environment == ""
	managedEnv, engineInstance, destinationName, err := a.reconcileManagedEnvironmentOfGitOpsDeployment(ctx, gitopsDeployment,
		gitopsDeplNamespace, isWorkspaceTarget)
	if err != nil {

		userError := "Unable to reconcile the ManagedEnvironment. Verify that the ManagedEnvironment and Secret are correctly defined, and have valid credentials"
		devError := fmt.Errorf("unable to get or create managed environment, isworkspacetarget:%v: %v", isWorkspaceTarget, err)

		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewUserDevError(userError, devError)

	}

	if engineInstance == nil {
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(fmt.Errorf("engine instance is nil when reconciling new GitOpsDeployment"))
	}

	appName := argosharedutil.GenerateArgoCDApplicationName(string(gitopsDeployment.UID))

	// If the user specified a value, always use it. If not, use the API resource namespace (but only in the workspace target case)
	destinationNamespace := gitopsDeployment.Spec.Destination.Namespace
	if isWorkspaceTarget {
		if destinationNamespace == "" {
			destinationNamespace = a.eventResourceNamespace
		}
	}
	if destinationNamespace == "" {
		userError := "the namespace specified in the destination field is invalid"
		devError := fmt.Errorf("invalid destination namespace: %s", destinationNamespace)

		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewUserDevError(userError, devError)
	}

	if _, err := a.sharedResourceEventLoop.ReconcileAppProjectRepositories(ctx, a.workspaceClient, gitopsDeplNamespace, a.log); err != nil {
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(fmt.Errorf("unexpected error on reconciling appproject repos: %w", err))
	}

	specFieldInput := argoCDSpecInput{
		crName:               appName,
		crNamespace:          engineInstance.Namespace_name,
		destinationNamespace: destinationNamespace,
		// TODO: GITOPSRVCE-66 - Fill this in with cluster credentials
		destinationName:      destinationName,
		sourceRepoURL:        gitopsDeployment.Spec.Source.RepoURL,
		sourcePath:           gitopsDeployment.Spec.Source.Path,
		sourceTargetRevision: gitopsDeployment.Spec.Source.TargetRevision,
		// syncOptions:       if non-empty, it gets updated below.
		automated: strings.EqualFold(gitopsDeployment.Spec.Type, managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated),
		project:   appProjectPrefix + clusterUser.Clusteruser_id,
	}

	// If AppProject-based isolation is disabled, then just default to using 'default' as the project field in the Argo CD Application
	if !sharedutil.AppProjectIsolationEnabled() {
		specFieldInput.project = "default"
	}

	if gitopsDeployment.Spec.SyncPolicy != nil && len(gitopsDeployment.Spec.SyncPolicy.SyncOptions) != 0 {
		userErr := checkValidSyncOption(gitopsDeployment.Spec.SyncPolicy.SyncOptions)

		if userErr != nil {
			return nil, nil, deploymentModifiedResult_Failed, userErr
		}

		specFieldInput.syncOptions = managedgitopsv1alpha1.SyncOptionToStringSlice(gitopsDeployment.Spec.SyncPolicy.SyncOptions)

	}

	specFieldText, err := createSpecField(specFieldInput)
	if err != nil {
		a.log.Error(err, "SEVERE: unable to marshal generated YAML")
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
	}

	var targetManagedEnvId string
	if managedEnv != nil {
		targetManagedEnvId = managedEnv.Managedenvironment_id
	}

	application := db.Application{
		Name:                    appName,
		Engine_instance_inst_id: engineInstance.Gitopsengineinstance_id,
		Managed_environment_id:  targetManagedEnvId,
		Spec_field:              specFieldText,
	}

	if err := dbQueries.CreateApplication(ctx, &application); err != nil {
		a.log.Error(err, "Unable to create application", application.GetAsLogKeyValues()...)

		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
	}
	a.log.Info("Created new Application in DB", application.GetAsLogKeyValues()...)

	// Create ApplicationOwner row in DB
	applicationOwner := &db.ApplicationOwner{
		ApplicationOwnerApplicationID: application.Application_id,
		ApplicationOwnerUserID:        clusterUser.Clusteruser_id,
	}

	// Fetch applicationOwner from database, if not found create it.
	if err := dbQueries.GetApplicationOwnerByApplicationID(ctx, applicationOwner); err != nil {
		if db.IsResultNotFoundError(err) {
			if err := dbQueries.CreateApplicationOwner(ctx, applicationOwner); err != nil {
				a.log.Error(err, "Unable to create application owner row in database", applicationOwner.GetAsLogKeyValues()...)

				return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
			}
			a.log.Info("Created new ApplicationOwner in DB", applicationOwner.GetAsLogKeyValues()...)

		} else {
			a.log.Error(err, "unable to retrieve applicationOwner", "applicationOwner", applicationOwner)
			return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
		}
	}

	requiredDeplToAppMapping := &db.DeploymentToApplicationMapping{
		Deploymenttoapplicationmapping_uid_id: string(gitopsDeployment.UID),
		Application_id:                        application.Application_id,
		DeploymentName:                        gitopsDeployment.Name,
		DeploymentNamespace:                   gitopsDeployment.Namespace,
		NamespaceUID:                          eventlooptypes.GetWorkspaceIDFromNamespaceID(gitopsDeplNamespace),
	}

	if _, err := dbutil.GetOrCreateDeploymentToApplicationMapping(ctx, requiredDeplToAppMapping, dbQueries, a.log); err != nil {
		a.log.Error(err, "unable to create deplToApp mapping", "deplToAppMapping", requiredDeplToAppMapping)

		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
	}

	dbOperationInput := db.Operation{
		Instance_id:   engineInstance.Gitopsengineinstance_id,
		Resource_id:   application.Application_id,
		Resource_type: db.OperationResourceType_Application,
	}

	gitopsEngineClient, err := a.k8sClientFactory.GetK8sClientForGitOpsEngineInstance(ctx, engineInstance)
	if err != nil {
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
	}

	waitForOperation := !a.testOnlySkipCreateOperation // if it's for a unit test, we don't wait for the operation
	if engineInstance == nil {
		err = fmt.Errorf("gitopsengineinstance is nil, expected non-nil:  %v", engineInstance)
		a.log.Error(err, "unexpected nil value of required objects")
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
	}
	if engineInstance.Namespace_name == "" {
		err = fmt.Errorf("gitopsengineinstance namespace is nil, expected non-nil:  %s", engineInstance.Gitopsengineinstance_id)
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
	}
	k8sOperation, dbOperation, err := operations.CreateOperation(ctx, waitForOperation, dbOperationInput,
		clusterUser.Clusteruser_id, engineInstance.Namespace_name, dbQueries, gitopsEngineClient, a.log)
	if err != nil {
		a.log.Error(err, "could not create operation", "operationNamespace", engineInstance.Namespace_name)
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
	}

	if err := operations.CleanupOperation(ctx, *dbOperation, *k8sOperation, dbQueries, gitopsEngineClient, !a.testOnlySkipCreateOperation, a.log); err != nil {
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
	}

	return &application, engineInstance, deploymentModifiedResult_Created, nil
}

// handleDeleteGitOpsDeplEvent handles GitOpsDeployment events where the user has just deleted a new GitOpsDeployment resource.
// In this case, we need to delete the Application and DeploymentToApplicationMapping rows in the database (among others).
//
// Finally, we need to inform the cluster-agent component, so that it configures Argo CD.
//
// Returns:
// - true if the goroutine responsible for this application can shutdown (e.g. because the GitOpsDeployment no longer exists, so no longer needs to be processed), false otherwise.
// - error is non-nil, if an error occurred
func (a applicationEventLoopRunner_Action) handleDeleteGitOpsDeplEvent(ctx context.Context, gitopsDepl *managedgitopsv1alpha1.GitOpsDeployment, clusterUser *db.ClusterUser,
	deplToAppMappingList *[]db.DeploymentToApplicationMapping,
	dbQueries db.ApplicationScopedQueries) (bool, gitopserrors.UserError) {

	if deplToAppMappingList == nil || clusterUser == nil { // sanity check
		return false, gitopserrors.NewDevOnlyError(fmt.Errorf("required parameter should not be nil in handleDelete: %v %v", deplToAppMappingList, clusterUser))
	}

	a.log.Info("Reconciling GitOpsDeployment that no longer exists (or never existed)")

	apiNamespace := corev1.Namespace{}
	if err := a.workspaceClient.Get(ctx, types.NamespacedName{Name: a.eventResourceNamespace}, &apiNamespace); err != nil {
		userError := "unable to retrieve the namespace containing the GitOpsDeployment"
		devError := fmt.Errorf("unable to retrieve workspace namespace")

		return false, gitopserrors.NewUserDevError(userError, devError)
	}

	var allErrors error

	signalShutdown := true

	// For each of the database entries that reference this gitopsdepl
	for idx := range *deplToAppMappingList {

		deplToAppMapping := (*deplToAppMappingList)[idx]

		// Clean up the database entries
		itemSignalledShutdown, err := a.cleanOldGitOpsDeploymentEntry(ctx, &deplToAppMapping, clusterUser, apiNamespace, dbQueries)
		if err != nil {
			// If we were unable to fully clean up a gitopsdeployment, then don't shutdown the goroutine
			signalShutdown = false

			if allErrors == nil {
				allErrors = fmt.Errorf("(error: %v)", err)
			} else {
				allErrors = fmt.Errorf("(error: %v) %v", err, allErrors)
			}
		}

		signalShutdown = signalShutdown && itemSignalledShutdown
	}

	if allErrors == nil {

		if gitopsDepl != nil && isGitOpsDeploymentDeleted(gitopsDepl) {
			// remove the finalizer if all the dependencies are cleaned up
			if err := removeFinalizerIfExists(ctx, a.workspaceClient, gitopsDepl, managedgitopsv1alpha1.DeletionFinalizer); err != nil {
				a.log.Error(err, "failed to remove the deletion finalizer from GitOpsDeployment")
				return false, gitopserrors.NewDevOnlyError(err)
			}
		}

		return signalShutdown, nil

	} else {
		return signalShutdown, gitopserrors.NewDevOnlyError(allErrors)
	}

}

func removeFinalizerIfExists(ctx context.Context, k8sClient client.Client, gitopsDepl *managedgitopsv1alpha1.GitOpsDeployment, finalizer string) error {
	if gitopsDepl == nil {
		return nil
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gitopsDepl), gitopsDepl)
		if err != nil {
			if apierr.IsNotFound(err) {
				return nil
			}
			return err
		}

		finalizers := removeItemFromSlice(finalizer, gitopsDepl.Finalizers)
		if len(finalizers) != len(gitopsDepl.Finalizers) {
			gitopsDepl.Finalizers = finalizers
			return k8sClient.Update(ctx, gitopsDepl)
		}

		return nil
	})
}

// we consider the GitOpsDeployment as deleted if:
// 1. the GitOpsDeployment object is not found.
// 2. the GitOpsDeployment is under deletion and the deletiontimestamp is set.
func isGitOpsDeploymentDeleted(gitopsDepl *managedgitopsv1alpha1.GitOpsDeployment) bool {
	return gitopsDepl == nil || gitopsDepl.DeletionTimestamp != nil
}

func removeItemFromSlice(item string, items []string) []string {
	result := []string{}
	for _, i := range items {
		if i != item {
			result = append(result, i)
		}
	}

	return result
}

// Note: this function will return a nil ManagedEnvironment and/or GitOpsEngineInstance if the ManagedEnvironment
// doesn't exist (for example, because it was deleted)
func (a applicationEventLoopRunner_Action) reconcileManagedEnvironmentOfGitOpsDeployment(ctx context.Context,
	gitopsDeployment managedgitopsv1alpha1.GitOpsDeployment, gitopsDeplNamespace corev1.Namespace,
	isWorkspaceTarget bool) (*db.ManagedEnvironment,
	*db.GitopsEngineInstance, string, error) {

	// Ask the event loop to ensure that the managed environment exists, is up-to-date, and is valid (can be connected to using k8s client)
	sharedResourceRes, err := a.sharedResourceEventLoop.ReconcileSharedManagedEnv(ctx, a.workspaceClient, gitopsDeplNamespace,
		gitopsDeployment.Spec.Destination.Environment, a.eventResourceNamespace, isWorkspaceTarget,
		a.k8sClientFactory, a.log)

	if err != nil {
		return nil, nil, "", fmt.Errorf("unable to get or create managed environment when reconciling for GitOpsDeployment: %v", err)
	}

	var destinationName string

	if isWorkspaceTarget {
		destinationName = sharedutil.ArgoCDDefaultDestinationInCluster

	} else {

		if sharedResourceRes.ManagedEnv != nil {
			destinationName = argosharedutil.GenerateArgoCDClusterSecretName(*sharedResourceRes.ManagedEnv)
		}
	}

	return sharedResourceRes.ManagedEnv, sharedResourceRes.GitopsEngineInstance, destinationName, nil
}

// handleUpdatedGitOpsDeplEvent handles GitOpsDeployment events where the user has updated an existing GitOpsDeployment resource.
// In this case, we need to ensure the Application row in the database is consistent with what the user has provided
// in the GitOpsDeployment.
//
// Finally, we need to inform the cluster-agent component, so that it configures Argo CD.
//
// Returns:
// - true if the goroutine responsible for this application can shutdown (e.g. because the GitOpsDeployment no longer exists, so no longer needs to be processed), false otherwise.
// - references to the Application and GitOpsEngineInstance database fields.
// - error is non-nil, if an error occurred
func (a applicationEventLoopRunner_Action) handleUpdatedGitOpsDeplEvent(ctx context.Context, deplToAppMapping *db.DeploymentToApplicationMapping,
	gitopsDeployment managedgitopsv1alpha1.GitOpsDeployment, clusterUser *db.ClusterUser,
	dbQueries db.ApplicationScopedQueries) (*db.Application, *db.GitopsEngineInstance, deploymentModifiedResult, gitopserrors.UserError) {

	if deplToAppMapping == nil || gitopsDeployment.UID == "" || clusterUser == nil {
		return nil, nil, deploymentModifiedResult_Failed,
			gitopserrors.NewDevOnlyError(fmt.Errorf("unexpected nil param in handleUpdatedGitOpsDeplEvent: %v %v %v",
				deplToAppMapping, gitopsDeployment, clusterUser))
	}

	log := a.log.WithValues(logutil.Log_ApplicationID, deplToAppMapping.Application_id, logutil.Log_K8s_Request_UID, gitopsDeployment.UID)

	log.Info("Received GitOpsDeployment event for an existing GitOpsDeployment resource")

	application := &db.Application{Application_id: deplToAppMapping.Application_id}
	if err := dbQueries.GetApplicationById(ctx, application); err != nil {
		if !db.IsResultNotFoundError(err) {
			log.Error(err, "unable to retrieve Application DB entry in handleUpdatedGitOpsDeplEvent")
			return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
		} else {

			// The application pointed to by the deplToAppMapping doesn't exist; this shouldn't happen.
			log.Error(err, "SEVERE: Application pointed to by deplToAppMapping doesn't exist, in handleUpdatedGitOpsDeplEvent")

			// Delete the deplToAppMapping, since the app doesn't exist. This should cause the gitopsdepl to be reconciled by the event loop.
			if _, err := dbQueries.DeleteDeploymentToApplicationMappingByDeplId(ctx, deplToAppMapping.Deploymenttoapplicationmapping_uid_id); err != nil {
				log.Error(err, "Unable to delete deplToAppMapping which pointed to non-existent Application, in handleUpdatedGitOpsDeplEvent")
				return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
			}

			log.Info("Deleted DeploymentToApplicationMapping with Deployment ID: " + deplToAppMapping.Deploymenttoapplicationmapping_uid_id)
			return nil, nil, deploymentModifiedResult_Deleted, gitopserrors.NewDevOnlyError(err)
		}
	}

	apiNamespace := corev1.Namespace{}
	if err := a.workspaceClient.Get(ctx, types.NamespacedName{Name: a.eventResourceNamespace}, &apiNamespace); err != nil {
		userError := "unable to retrieve namespace containing the GitOpsDeployment"
		devError := fmt.Errorf("unable to retrieve workspace namespace")
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewUserDevError(userError, devError)
	}

	isWorkspaceTarget := gitopsDeployment.Spec.Destination.Environment == ""
	managedEnv, engineInstance, destinationName, err := a.reconcileManagedEnvironmentOfGitOpsDeployment(ctx, gitopsDeployment, apiNamespace, isWorkspaceTarget)
	if err != nil {
		userError := "unable to reconcile the ManagedEnvironment resource. Ensure that the ManagedEnvironment exists, it references a Secret, and the Secret is valid"
		devError := fmt.Errorf("unable to get or create managed environment: %v", err)
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewUserDevError(userError, devError)
	}

	if engineInstance == nil {
		// If engineInstance from reconcileManagedEnvironmentOfGitOpsDeployment is nil, instead get the engine instance from
		// the application.
		engineInstance = &db.GitopsEngineInstance{
			Gitopsengineinstance_id: application.Engine_instance_inst_id,
		}
		if err := dbQueries.GetGitopsEngineInstanceById(ctx, engineInstance); err != nil {
			return nil, nil, deploymentModifiedResult_Failed,
				gitopserrors.NewDevOnlyError(fmt.Errorf("unable to retrieve GitOpsEngineInstance for existing GitOpsDeployment: %v", err))
		}
	}

	// Sanity check that the application.name matches the expected value set in handleCreateGitOpsEvent
	expectedAppName := argosharedutil.GenerateArgoCDApplicationName(string(gitopsDeployment.UID))
	if expectedAppName != application.Name {
		log.Error(nil, "SEVERE: The name of the Argo CD Application CR should remain constant")
		return nil, nil, deploymentModifiedResult_Failed,
			gitopserrors.NewDevOnlyError(fmt.Errorf("name of the Argo CD Application should not change:'%s' '%s'", expectedAppName, application.Name))
	}

	// If the user specified a value, always use it. If not, use the API resource namespace (but only in the workspace target case)
	destinationNamespace := gitopsDeployment.Spec.Destination.Namespace
	if isWorkspaceTarget {
		if destinationNamespace == "" {
			destinationNamespace = a.eventResourceNamespace
		}
	}

	if destinationNamespace == "" {
		userError := "destination namespace specified in the .spec.Destination.Namespace field is invalid"
		devError := fmt.Errorf("invalid destination namespace: %s", destinationNamespace)
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewUserDevError(userError, devError)
	}

	appProjectDBRowsUpdated, err := a.sharedResourceEventLoop.ReconcileAppProjectRepositories(ctx, a.workspaceClient, apiNamespace, log)

	if err != nil {
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(fmt.Errorf("unexpected error on reconciling appproject repos: %w", err))
	}

	specFieldInput := argoCDSpecInput{
		crName:               application.Name,
		crNamespace:          engineInstance.Namespace_name,
		destinationNamespace: destinationNamespace,
		destinationName:      destinationName,
		sourceRepoURL:        gitopsDeployment.Spec.Source.RepoURL,
		sourcePath:           gitopsDeployment.Spec.Source.Path,
		sourceTargetRevision: gitopsDeployment.Spec.Source.TargetRevision,
		// syncOptions:       if non-empty, it gets updated below.
		automated: strings.EqualFold(gitopsDeployment.Spec.Type, managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated),
		project:   appProjectPrefix + clusterUser.Clusteruser_id,
	}

	// If AppProject-based isolation is disabled, then just default to using 'default' as the project field in the Argo CD Application
	if !sharedutil.AppProjectIsolationEnabled() {
		specFieldInput.project = "default"
	}

	if gitopsDeployment.Spec.SyncPolicy != nil && len(gitopsDeployment.Spec.SyncPolicy.SyncOptions) != 0 {
		if err := checkValidSyncOption(gitopsDeployment.Spec.SyncPolicy.SyncOptions); err != nil {
			return nil, nil, deploymentModifiedResult_Failed, err
		}

		specFieldInput.syncOptions = managedgitopsv1alpha1.SyncOptionToStringSlice(gitopsDeployment.Spec.SyncPolicy.SyncOptions)
	}

	shouldUpdateApplication := false

	if appProjectDBRowsUpdated {
		shouldUpdateApplication = true
	}

	// If the spec field changed from what is in the database, we should update the application
	{
		specFieldResult, err := createSpecField(specFieldInput)
		if err != nil {
			log.Error(err, "SEVERE: Unable to parse generated spec field")
			return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
		}

		if specFieldResult == application.Spec_field {
			log.Info("Processed GitOpsDeployment event: No spec change detected between Application DB entry and GitOpsDeployment CR")
			// No change required: the application database entry is consistent with the gitopsdepl CR
		} else {

			shouldUpdateApplication = true
			application.Spec_field = specFieldResult

			log.Info("Processed GitOpsDeployment event: Spec change detected between Application DB entry and GitOpsDeployment CR")
		}

		// If the managed environment changed from what is in the database, we should update the environment
		var newManagedEnvId string
		if managedEnv != nil {
			newManagedEnvId = managedEnv.Managedenvironment_id
		}
		if newManagedEnvId != application.Managed_environment_id {
			application.Managed_environment_id = newManagedEnvId
			shouldUpdateApplication = true
		}
	}

	// If neither the managed environment, nor the spec field changed, then no need to update the database, so exit.
	if !shouldUpdateApplication {
		log.Info("Processed GitOpsDeployment event: No Application row change detected")
		return application, engineInstance, deploymentModifiedResult_NoChange, nil
	}

	if err := dbQueries.UpdateApplication(ctx, application); err != nil {
		log.Error(err, "Unable to update application, after mismatch detected")

		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
	}
	log.Info("Processed GitOpsDeployment event: Application updated in database from latest API changes")

	applicationOwner := &db.ApplicationOwner{
		ApplicationOwnerApplicationID: application.Application_id,
		ApplicationOwnerUserID:        clusterUser.Clusteruser_id,
	}

	// Fetch applicationOwner from database, if not found create it.
	if err := dbQueries.GetApplicationOwnerByApplicationID(ctx, applicationOwner); err != nil {
		if db.IsResultNotFoundError(err) {
			if err := dbQueries.CreateApplicationOwner(ctx, applicationOwner); err != nil {
				log.Error(err, "Unable to create application owner row in database", applicationOwner.GetAsLogKeyValues()...)

				return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
			}
			log.Info("Created new Application Owner in DB", applicationOwner.GetAsLogKeyValues()...)

		} else {
			log.Error(err, "unable to retrieve applicationOwner", "applicationOwner", applicationOwner)
			return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
		}
	}
	log.Info("Processed GitOpsDeployment event: Application owner exists after updating application in database")

	// Create the operation
	gitopsEngineClient, err := a.k8sClientFactory.GetK8sClientForGitOpsEngineInstance(ctx, engineInstance)
	if err != nil {
		log.Error(err, "unable to retrieve gitopsengineinstance for updated gitopsdepl", "gitopsEngineInstance", engineInstance.EngineCluster_id)
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
	}

	dbOperationInput := db.Operation{
		Instance_id:   engineInstance.Gitopsengineinstance_id,
		Resource_id:   application.Application_id,
		Resource_type: db.OperationResourceType_Application,
	}

	waitForOperation := !a.testOnlySkipCreateOperation // if it's for a unit test, we don't wait for the operation
	if engineInstance.Namespace_name == "" {
		err = fmt.Errorf("gitopsengineinstance namespace is nil, expected non-nil:  %s", engineInstance.Gitopsengineinstance_id)
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
	}
	k8sOperation, dbOperation, err := operations.CreateOperation(ctx, waitForOperation, dbOperationInput, clusterUser.Clusteruser_id,
		engineInstance.Namespace_name, dbQueries, gitopsEngineClient, log)
	if err != nil {
		log.Error(err, "could not create operation")
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
	}

	if err := operations.CleanupOperation(ctx, *dbOperation, *k8sOperation, dbQueries, gitopsEngineClient, !a.testOnlySkipCreateOperation, log); err != nil {
		return nil, nil, deploymentModifiedResult_Failed, gitopserrors.NewDevOnlyError(err)
	}

	return application, engineInstance, deploymentModifiedResult_Updated, nil

}

// Returns true/false if the GitOpsDeployment was successfully cleaned up (and thus the runner can be shutdown), and an error
func (a applicationEventLoopRunner_Action) cleanOldGitOpsDeploymentEntry(ctx context.Context,
	deplToAppMapping *db.DeploymentToApplicationMapping, clusterUser *db.ClusterUser,
	apiNamespace corev1.Namespace, dbQueries db.ApplicationScopedQueries) (bool, error) {

	const (
		signalledShutdown_true  = true
		signalledShutdown_false = false
	)

	dbApplicationFound := true

	// If the gitopsdepl CR doesn't exist, but database row does, then the CR has been deleted, so handle it.
	dbApplication := db.Application{
		Application_id: deplToAppMapping.Application_id,
	}
	if err := dbQueries.GetApplicationById(ctx, &dbApplication); err != nil {

		a.log.Error(err, "unable to get application by id", logutil.Log_ApplicationID, deplToAppMapping.Application_id)

		if db.IsResultNotFoundError(err) {
			dbApplicationFound = false
			// Log the error, but continue.
		} else {
			return signalledShutdown_false, err
		}
	}

	log := a.log.WithValues(logutil.Log_ApplicationID, deplToAppMapping.Application_id)

	// 1) Remove the ApplicationState from the database
	rowsDeleted, err := dbQueries.DeleteApplicationStateById(ctx, deplToAppMapping.Application_id)
	if err != nil {

		log.V(logutil.LogLevel_Warn).Error(err, "unable to delete application state by id")
		return signalledShutdown_false, err

	} else if rowsDeleted == 0 {
		// Log the warning, but continue
		log.Info("No ApplicationState rows were found, while cleaning up after deleted GitOpsDeployment", "rowsDeleted", rowsDeleted)
	} else {
		log.Info("ApplicationState rows were successfully deleted, while cleaning up after deleted GitOpsDeployment", "rowsDeleted", rowsDeleted)
	}

	// 2) Set the application field of SyncOperations to nil, for all SyncOperations that point to this Application
	// - this ensures that the foreign key constraint of SyncOperation doesn't prevent us from deletion the Application
	rowsUpdated, err := dbQueries.UpdateSyncOperationRemoveApplicationField(ctx, deplToAppMapping.Application_id)
	if err != nil {
		log.Error(err, "unable to update old sync operations", logutil.Log_ApplicationID, deplToAppMapping.Application_id)
		return signalledShutdown_false, err

	} else if rowsUpdated == 0 {
		log.Info("No SyncOperation rows updated, for updating old syncoperations on GitOpsDeployment deletion")
	} else {
		log.Info("Removed references to Application from all SyncOperations that reference it")
	}

	// 3) Delete DeplToAppMapping row that points to this Application
	rowsDeleted, err = dbQueries.DeleteDeploymentToApplicationMappingByDeplId(ctx, deplToAppMapping.Deploymenttoapplicationmapping_uid_id)
	if err != nil {
		log.Error(err, "unable to delete deplToAppMapping by id", "deplToAppMapUid", deplToAppMapping.Deploymenttoapplicationmapping_uid_id)
		return signalledShutdown_false, err

	} else if rowsDeleted == 0 {
		// Log the warning, but continue
		log.V(logutil.LogLevel_Warn).Error(nil, "unexpected number of rows deleted for deplToAppMapping", "rowsDeleted", rowsDeleted)
	} else {
		log.Info("While cleaning up after deleted GitOpsDeployment, deleted deplToAppMapping", "deplToAppMapUid", deplToAppMapping.Deploymenttoapplicationmapping_uid_id)
	}

	if !dbApplicationFound {
		log.Info("While cleaning up old gitopsdepl entries, the Application row wasn't found. No more work to do.")
		// If the Application CR no longer exists, then our work is done.
		return signalledShutdown_true, nil
	}

	// 4) Remove ApplicationOwner from database
	log.Info("GitOpsDeployment was deleted, so deleting ApplicationOwner row from database")
	rowsDeleted, err = dbQueries.DeleteApplicationOwner(ctx, deplToAppMapping.Application_id)
	if err != nil {
		// Log the error, but continue
		log.Error(err, "unable to delete application owner by id")
	} else if rowsDeleted == 0 {
		// Log the error, but continue
		log.V(logutil.LogLevel_Warn).Error(nil, "unexpected number of rows deleted for application owner ", "rowsDeleted", rowsDeleted)
	}

	// If the Application table entry still exists, finish the cleanup...

	// 5) Remove the Application from the database
	log.Info("GitOpsDeployment was deleted, so deleting Application row from database")
	rowsDeleted, err = dbQueries.DeleteApplicationById(ctx, deplToAppMapping.Application_id)
	if err != nil {
		// Log the error, but continue
		log.Error(err, "unable to delete application by id")
	} else if rowsDeleted == 0 {
		// Log the error, but continue
		log.V(logutil.LogLevel_Warn).Error(nil, "unexpected number of rows deleted for application", "rowsDeleted", rowsDeleted)
	}

	specFieldAppFromDB := fauxargocd.FauxApplication{}

	if err := yaml.Unmarshal([]byte(dbApplication.Spec_field), &specFieldAppFromDB); err != nil {
		log.Error(err, "SEVERE: unable to unmarshal DB application spec field")
		return signalledShutdown_false, err
	}

	// Ensure the the AppProjectRepository database entries accurately reflect the current state of the GitOpsDeployments/RepositoryCredentials in the Namespace
	if _, err := a.sharedResourceEventLoop.ReconcileAppProjectRepositories(ctx, a.workspaceClient, apiNamespace, log); err != nil {
		log.Error(err, "unable to reconcile app project repositories")
		return signalledShutdown_false, err
	}

	gitopsEngineInstance, err := a.sharedResourceEventLoop.GetGitopsEngineInstanceById(ctx, dbApplication.Engine_instance_inst_id, a.workspaceClient, apiNamespace, a.log)
	if err != nil {
		log := log.WithValues("gitopsEngineID", dbApplication.Engine_instance_inst_id)

		if db.IsResultNotFoundError(err) {
			log.Error(err, "GitOpsEngineInstance could not be retrieved during gitopsdepl deletion handling")
			return signalledShutdown_false, err
		} else {
			log.Error(err, "Error occurred on attempting to retrieve gitops engine instance")
			return signalledShutdown_false, err
		}
	}

	// 6) Now that we've deleted the Application row, create the operation that will cause the Argo CD application
	// to be deleted.
	gitopsEngineClient, err := a.k8sClientFactory.GetK8sClientForGitOpsEngineInstance(ctx, gitopsEngineInstance)
	if err != nil {
		log.Error(err, "could not retrieve client for gitops engine instance", "instance", gitopsEngineInstance.Gitopsengineinstance_id)
		return signalledShutdown_false, err
	}
	dbOperationInput := db.Operation{
		Instance_id:   dbApplication.Engine_instance_inst_id,
		Resource_id:   deplToAppMapping.Application_id,
		Resource_type: db.OperationResourceType_Application,
	}

	waitForOperation := !a.testOnlySkipCreateOperation // if it's for a unit test, we don't wait for the operation
	if gitopsEngineInstance == nil {
		err = fmt.Errorf("gitopsengineinstance is nil, expected non-nil:  %v", gitopsEngineInstance)
		log.Error(err, "unexpected nil value of required objects")
		return signalledShutdown_false, err
	}

	if gitopsEngineInstance.Namespace_name == "" {
		err = fmt.Errorf("gitopsengineinstance namespace is nil, expected non-nil:  %s", gitopsEngineInstance.Gitopsengineinstance_id)
		return signalledShutdown_false, err
	}
	k8sOperation, dbOperation, err := operations.CreateOperation(ctx, waitForOperation, dbOperationInput,
		clusterUser.Clusteruser_id, gitopsEngineInstance.Namespace_name, dbQueries, gitopsEngineClient, log)
	if err != nil {
		log.Error(err, "unable to create operation", "operation", dbOperationInput.ShortString())
		return signalledShutdown_false, err
	}

	// 7) Finally, clean up the operation
	if err := operations.CleanupOperation(ctx, *dbOperation, *k8sOperation, dbQueries, gitopsEngineClient, !a.testOnlySkipCreateOperation, log); err != nil {
		log.Error(err, "unable to cleanup operation", "operation", dbOperationInput.ShortString())
		return signalledShutdown_false, err
	}

	return signalledShutdown_true, nil

}

// applicationEventRunner_handleUpdateDeploymentStatusTick updates the status field of all the GitOpsDeploymentCRs in the workspace.
func (a *applicationEventLoopRunner_Action) applicationEventRunner_handleUpdateDeploymentStatusTick(ctx context.Context,
	resourceName string, namespaceName string, dbQueries db.ApplicationScopedQueries) (bool, error) {

	const (
		crUpdated_true  = true  // true: the GitOpsDeployment.status field was updated
		crUpdated_false = false // false: not updated
	)

	// TODO: GITOPSRVCE-68 - PERF - In general, polling for all GitOpsDeployments in a workspace will scale poorly with large number of applications in the workspace. We should switch away from polling in the future.

	// 1) Retrieve the GitOpsDeployment from the namespace, using the namespace and resource name
	gitopsDeployment := &managedgitopsv1alpha1.GitOpsDeployment{}
	{
		gitopsDeploymentKey := client.ObjectKey{Namespace: namespaceName, Name: resourceName}

		if err := a.workspaceClient.Get(ctx, gitopsDeploymentKey, gitopsDeployment); err != nil {

			if apierr.IsNotFound(err) {
				// Our work is done
				return crUpdated_false, nil
			} else {
				a.log.Error(err, "unable to locate gitops object in handleUpdateDeploymentStatusTick")
				return crUpdated_false, err
			}
		}
	}

	// Copy of the GitOpsDeployment we retrieved, before its modified below
	originalGitOpsDeployment := *gitopsDeployment.DeepCopy()

	// 2) Retrieve the DTAM for the GitOpsDeployment, if it exists.
	mapping := db.DeploymentToApplicationMapping{
		Deploymenttoapplicationmapping_uid_id: string(gitopsDeployment.UID),
	}
	if err := dbQueries.GetDeploymentToApplicationMappingByDeplId(ctx, &mapping); err != nil {

		if db.IsResultNotFoundError(err) {
			// No Application associated with this GitOpsDeployment, so no work to do
			return crUpdated_false, nil
		} else {
			a.log.Error(err, "unable to retrieve DeploymentToApplicationMapping in tick status update")
			return crUpdated_false, err
		}
	}

	// 3) Retrieve the application state for the application pointed to by the DTAM
	applicationState := db.ApplicationState{Applicationstate_application_id: mapping.Application_id}
	if err := dbQueries.GetApplicationStateById(ctx, &applicationState); err != nil {

		if db.IsResultNotFoundError(err) {
			a.log.Info("ApplicationState not found for application, on deploymentStatusTick: " + applicationState.Applicationstate_application_id)
			return crUpdated_false, nil
		} else {
			return crUpdated_false, err
		}
	}

	appStatus, err := decompressApplicationStatus(applicationState.ArgoCD_Application_Status)
	if err != nil {
		a.log.Error(err, "unable to decompress resources byte array received from table.")
		return crUpdated_false, err
	}

	// 4) Update the health and status field of the GitOpsDepl CR

	// Update the gitopsDeployment instance with health and status values (fetched from the database)
	gitopsDeployment.Status.Health.Status = managedgitopsv1alpha1.HealthStatusCode(appStatus.Health.Status)
	gitopsDeployment.Status.Health.Message = appStatus.Health.Message
	gitopsDeployment.Status.Sync.Status = managedgitopsv1alpha1.SyncStatusCode(appStatus.Sync.Status)
	gitopsDeployment.Status.Sync.Revision = appStatus.Sync.Revision

	// We update the GitopsDeployment .status.conditions with the conditions from the Argo CD Application, if the conditions column of ApplicationState row is non empty.
	newGitopsDeplConditions := []managedgitopsv1alpha1.GitOpsDeploymentCondition{}
	if len(appStatus.Conditions) != 0 {
		for _, cond := range appStatus.Conditions {
			newGitopsDeplConditions = append(newGitopsDeplConditions, managedgitopsv1alpha1.GitOpsDeploymentCondition{
				Type:    managedgitopsv1alpha1.GitOpsDeploymentConditionType(cond.Type),
				Message: cond.Message,
				Reason:  managedgitopsv1alpha1.GitOpsDeploymentReasonType(cond.Type),
			})
		}
	}

	conditionManager := condition.NewConditionManager()
	for _, c := range newGitopsDeplConditions {
		// If the new condition already exists, then update it with the latest values.
		conditionManager.SetCondition(&gitopsDeployment.Status.Conditions, c.Type, managedgitopsv1alpha1.GitOpsConditionStatusTrue, managedgitopsv1alpha1.GitOpsDeploymentReasonType(c.Type), c.Message)
	}

	// Go through the existing conditions and check if they are present in the list of new conditions. If they are absent then it can marked as resolved.
	for _, c := range gitopsDeployment.Status.Conditions {
		reason := c.Type + "Resolved"
		if !conditionManager.HasCondition(&newGitopsDeplConditions, c.Type) && c.Reason != managedgitopsv1alpha1.GitOpsDeploymentReasonType(reason) {
			conditionManager.SetCondition(&gitopsDeployment.Status.Conditions, c.Type, managedgitopsv1alpha1.GitOpsConditionStatusFalse, managedgitopsv1alpha1.GitOpsDeploymentReasonType(reason), "")
		}
	}

	gitopsDeployment.Status.Resources = extractResourceStatus(appStatus.Resources)

	gitopsDeployment.Status.OperationState, err = extractOperationState(appStatus.OperationState)
	if err != nil {
		a.log.Error(err, "unable to extract operationState from ApplicationState table.")
		return crUpdated_false, err
	}

	comparedTo := appStatus.Sync.ComparedTo

	// If the `comparedTo` value from Argo CD has a non-empty destination name field, then retrieve the corresponding `GitOpsDeploymentManagedEnvironment` resource that has that name,
	// and include the name of that resource in what we return in this API.
	if comparedTo.Destination.Name != "" {
		apiCRToDBMapping := &db.APICRToDatabaseMapping{
			APIResourceType: db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentManagedEnvironment,
			DBRelationType:  db.APICRToDatabaseMapping_DBRelationType_ManagedEnvironment,
			DBRelationKey:   comparedTo.Destination.Name,
		}

		if err := dbQueries.GetAPICRForDatabaseUID(ctx, apiCRToDBMapping); err != nil {
			// If any error occurs while we're trying to retrieve the value of this field, we just report the
			// value as empty: if necessary, it will be updated on the next tick.
			comparedTo.Destination.Name = ""
		} else {
			comparedTo.Destination.Name = apiCRToDBMapping.APIResourceName
		}
	}

	// Update gitopsDeployment status with reconciledState
	gitopsDeployment.Status.ReconciledState.Source.Path = comparedTo.Source.Path
	gitopsDeployment.Status.ReconciledState.Source.RepoURL = comparedTo.Source.RepoURL
	gitopsDeployment.Status.ReconciledState.Source.Branch = comparedTo.Source.TargetRevision
	gitopsDeployment.Status.ReconciledState.Destination.Name = comparedTo.Destination.Name
	gitopsDeployment.Status.ReconciledState.Destination.Namespace = comparedTo.Destination.Namespace

	// If nothing has changed in the status field, our work is done.
	if reflect.DeepEqual(gitopsDeployment.Status, originalGitOpsDeployment.Status) {
		return crUpdated_false, nil
	}
	// Update the actual object in Kubernetes
	if err := a.workspaceClient.Status().Update(ctx, gitopsDeployment, &client.UpdateOptions{}); err != nil {
		return crUpdated_false, err
	}
	// We don't need to log status updates, e.g. via 'sharedutil.LogAPIResourceChangeEvent'

	a.log.V(logutil.LogLevel_Debug).Info("Updated status in deploymentStatusTick")

	// NOTE: make sure to preserve the existing conditions fields that are in the status field of the CR, when updating the status!

	return crUpdated_true, nil

}

// gitOpsDeploymentAdapter is an "adapter" for GitOpsDeployment allowing you to easily plug any other related
// API component (i.e. for adding Conditions, look at setGitOpsDeploymentCondition() method)
// Same principle can be used for others, e.g. Finalizers, or any other field which is part of the GitOpsDeployment CRD
// It comes as a bundle along with a logger, client and ctx, so it can be easily adapted to the code
type gitOpsDeploymentAdapter struct {
	gitOpsDeployment *managedgitopsv1alpha1.GitOpsDeployment
	logger           logr.Logger
	client           client.Client
	conditionManager condition.Conditions
	ctx              context.Context
}

// newGitOpsDeploymentAdapter returns an initialized gitOpsDeploymentAdapter
func newGitOpsDeploymentAdapter(gitopsDeployment *managedgitopsv1alpha1.GitOpsDeployment, l logr.Logger, client client.Client, manager condition.Conditions, ctx context.Context) *gitOpsDeploymentAdapter {
	return &gitOpsDeploymentAdapter{
		gitOpsDeployment: gitopsDeployment,
		logger:           l,
		client:           client,
		conditionManager: manager,
		ctx:              ctx,
	}
}

// getMatchingGitOpsDeployment returns an updated instance of GitOpsDeployment obj from Kubernetes
func getMatchingGitOpsDeployment(ctx context.Context, name, namespace string, k8sClient client.Client) (*managedgitopsv1alpha1.GitOpsDeployment, error) {
	gitopsDepl := &managedgitopsv1alpha1.GitOpsDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}

	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gitopsDepl), gitopsDepl); err != nil {
		return &managedgitopsv1alpha1.GitOpsDeployment{}, err
	}

	return gitopsDepl, nil
}

// setGitOpsDeploymentCondition calls SetCondition() with GitOpsDeployment conditions
func (g *gitOpsDeploymentAdapter) setGitOpsDeploymentCondition(conditionType managedgitopsv1alpha1.GitOpsDeploymentConditionType,
	reason managedgitopsv1alpha1.GitOpsDeploymentReasonType, errMessage gitopserrors.UserError) error {

	conditions := &g.gitOpsDeployment.Status.Conditions

	// Create a new condition and update the object in k8s with the error message, if err does exist
	if errMessage != nil {

		// We should only show user errors here. If the error doesn't contain a user error (only a dev error),
		// then just return UnknownError.
		userError := errMessage.UserError()
		if userError == "" {
			userError = gitopserrors.UnknownError
		}

		g.conditionManager.SetCondition(conditions, conditionType, managedgitopsv1alpha1.GitOpsConditionStatus(corev1.ConditionTrue),
			reason, userError)
		return g.client.Status().Update(g.ctx, g.gitOpsDeployment, &client.UpdateOptions{})

	} else {
		// if error does not exist, check if the condition exists or not
		if g.conditionManager.HasCondition(conditions, conditionType) {
			reason = reason + "Resolved"
			// Check the condition and mark it as resolved, if it's resolved
			if cond, _ := g.conditionManager.FindCondition(conditions, conditionType); cond.Reason != reason {
				g.conditionManager.SetCondition(conditions, conditionType,
					managedgitopsv1alpha1.GitOpsConditionStatus(corev1.ConditionFalse), reason, "")

				return g.client.Status().Update(g.ctx, g.gitOpsDeployment, &client.UpdateOptions{})
			}
			// do nothing, if the condition is already marked as resolved
		}
		// do nothing, if the condition does not exist anymore
	}

	return nil
}

func checkValidSyncOption(syncOptions []managedgitopsv1alpha1.SyncOption) gitopserrors.UserError {

	for _, syncOptionString := range syncOptions {

		match := true

		// Check for SyncOption string
		if !(syncOptionString == managedgitopsv1alpha1.SyncOptions_CreateNamespace_true ||
			syncOptionString == managedgitopsv1alpha1.SyncOptions_CreateNamespace_false) {
			match = false
		}

		// If at least one of the options don't match, return a user  error
		if !match {
			userError := "the specified sync option in .spec.syncPolicy.syncOptions is either mispelled or is not supported by GitOpsDeployment"
			devError := fmt.Errorf("invalid SyncOption : %s", syncOptionString)

			return gitopserrors.NewUserDevError(userError, devError)
		}
	}
	return nil
}

type argoCDSpecInput struct {
	// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!
	crName      string
	crNamespace string
	// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!
	destinationNamespace string
	destinationName      string
	// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!
	sourceRepoURL        string
	sourcePath           string
	sourceTargetRevision string
	syncOptions          []string
	// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!
	automated bool
	// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!
	project string

	// Hopefully you are getting the message, here :)
}

func createSpecField(fieldsParam argoCDSpecInput) (string, error) {

	sanitize := func(input string) string {
		input = strings.ReplaceAll(input, "\"", "")
		input = strings.ReplaceAll(input, "'", "")
		input = strings.ReplaceAll(input, "`", "")
		input = strings.ReplaceAll(input, "\r", "")
		input = strings.ReplaceAll(input, "\n", "")
		input = strings.ReplaceAll(input, "&", "")
		input = strings.ReplaceAll(input, ";", "")
		input = strings.ReplaceAll(input, "%", "")

		return input
	}

	sanitizeArray := func(input []string) []string {
		res := []string{}
		for _, syncOptionVar := range input {
			res = append(res, sanitize(syncOptionVar))
		}
		return res
	}

	fields := argoCDSpecInput{
		// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!
		crName:               sanitize(fieldsParam.crName),
		crNamespace:          sanitize(fieldsParam.crNamespace),
		destinationNamespace: sanitize(fieldsParam.destinationNamespace),
		// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!
		destinationName:      sanitize(fieldsParam.destinationName),
		sourceRepoURL:        sanitize(fieldsParam.sourceRepoURL),
		sourcePath:           sanitize(fieldsParam.sourcePath),
		sourceTargetRevision: sanitize(fieldsParam.sourceTargetRevision),
		syncOptions:          sanitizeArray(fieldsParam.syncOptions),
		automated:            fieldsParam.automated,
		project:              sanitize(fieldsParam.project),
		// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!
		// Hopefully you are getting the message, here :)
	}

	application := fauxargocd.FauxApplication{
		FauxTypeMeta: fauxargocd.FauxTypeMeta{
			Kind:       "Application",
			APIVersion: "argoproj.io/v1alpha1",
		},
		FauxObjectMeta: fauxargocd.FauxObjectMeta{
			Name:      fields.crName,
			Namespace: fields.crNamespace,
		},
		Spec: fauxargocd.FauxApplicationSpec{
			Source: fauxargocd.ApplicationSource{
				RepoURL:        fields.sourceRepoURL,
				Path:           fields.sourcePath,
				TargetRevision: fields.sourceTargetRevision,
			},
			Destination: fauxargocd.ApplicationDestination{
				Name:      fields.destinationName,
				Namespace: fields.destinationNamespace,
			},
			Project: fields.project,
		},
	}

	if fields.automated {
		application.Spec.SyncPolicy = &fauxargocd.SyncPolicy{
			Automated: &fauxargocd.SyncPolicyAutomated{
				Prune:      true,
				SelfHeal:   true,
				AllowEmpty: true,
			},
			SyncOptions: fauxargocd.SyncOptions{
				prunePropagationPolicy,
			},
			Retry: &fauxargocd.RetryStrategy{
				Limit: -1,
				Backoff: &fauxargocd.Backoff{
					Duration:    "5s",
					Factor:      getInt64Pointer(2),
					MaxDuration: "3m",
				},
			},
		}

	} else {
		// !fields.automated
		application.Spec.SyncPolicy = nil
	}

	if len(fields.syncOptions) > 0 {

		if application.Spec.SyncPolicy == nil {
			application.Spec.SyncPolicy = &fauxargocd.SyncPolicy{}
		}

		if application.Spec.SyncPolicy.SyncOptions == nil {
			application.Spec.SyncPolicy.SyncOptions = fauxargocd.SyncOptions{}
		}

		for _, syncOptionString := range fields.syncOptions {
			application.Spec.SyncPolicy.SyncOptions = append(application.Spec.SyncPolicy.SyncOptions,
				syncOptionString)
		}
	}

	resBytes, err := goyaml.Marshal(application)
	if err != nil {
		return "", err
	}

	return string(resBytes), nil
}

// Decompress byte array received from ApplicationState table and convert it into Application status.
func decompressApplicationStatus(statusBytes []byte) (*fauxargocd.FauxApplicationStatus, error) {
	appStatus := &fauxargocd.FauxApplicationStatus{}
	applStatusBytes, err := sharedutil.DecompressObject(statusBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress Application status: %v", err)
	}

	err = goyaml.Unmarshal(applStatusBytes, appStatus)
	if err != nil {
		return nil, fmt.Errorf("unable to Unmarshal Application status: %v", err)
	}

	return appStatus, nil
}

func extractResourceStatus(resources []fauxargocd.ResourceStatus) []managedgitopsv1alpha1.ResourceStatus {
	resourceStatus := make([]managedgitopsv1alpha1.ResourceStatus, len(resources))

	for index, resource := range resources {
		resourceStatus[index] = managedgitopsv1alpha1.ResourceStatus{
			Group:     resource.Group,
			Version:   resource.Version,
			Kind:      resource.Kind,
			Namespace: resource.Namespace,
			Name:      resource.Name,
			Status:    managedgitopsv1alpha1.SyncStatusCode(resource.Status),
		}
		if resource.Health != nil {
			resourceStatus[index].Health = &managedgitopsv1alpha1.HealthStatus{
				Status:  managedgitopsv1alpha1.HealthStatusCode(resource.Health.Status),
				Message: resource.Health.Message,
			}
		}
	}

	return resourceStatus
}

func extractOperationState(fauxOpState *fauxargocd.OperationState) (*managedgitopsv1alpha1.OperationState, error) {
	if fauxOpState == nil {
		return nil, nil
	}

	opStateBytes, err := json.Marshal(fauxOpState)
	if err != nil {
		return nil, err
	}

	opState := &managedgitopsv1alpha1.OperationState{}
	err = json.Unmarshal(opStateBytes, opState)
	if err != nil {
		return nil, err
	}

	return opState, nil
}

func getInt64Pointer(i int) *int64 {
	i64 := int64(i)
	return &i64
}

// isManagedEnvironmentConnectionUserError returns true if the error is likely an error in the cluster credentials provided by the user
func IsManagedEnvironmentConnectionUserError(err error, log logr.Logger) bool {

	if err == nil {
		return false
	}

	errStr := err.Error()

	return strings.Contains(errStr, shared_resource_loop.UnableToCreateRestConfigError)
}
