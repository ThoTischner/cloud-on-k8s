// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package license

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/elastic/cloud-on-k8s/pkg/controller/common/version"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	esclient "github.com/elastic/cloud-on-k8s/pkg/controller/elasticsearch/client"
	"github.com/elastic/cloud-on-k8s/pkg/controller/elasticsearch/label"
	"github.com/elastic/cloud-on-k8s/pkg/controller/elasticsearch/sset"
	"github.com/elastic/cloud-on-k8s/pkg/utils/compare"
	"github.com/elastic/cloud-on-k8s/pkg/utils/maps"

	esv1 "github.com/elastic/cloud-on-k8s/pkg/apis/elasticsearch/v1"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/license"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/operator"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/reconciler"
	"github.com/elastic/cloud-on-k8s/pkg/utils/k8s"
)

const (
	name = "license-controller"

	// defaultSafetyMargin is the duration used by this controller to ensure licenses are updated well before expiry
	// In case of any operational issues affecting this controller clusters will have enough runway on their current license.
	defaultSafetyMargin  = 30 * 24 * time.Hour
	minimumRetryInterval = 1 * time.Hour
)

var log = logf.Log.WithName(name)

// Reconcile reads the cluster license for the cluster being reconciled. If found, it checks whether it is still valid.
// If there is none it assigns a new one.
// In any case it schedules a new reconcile request to be processed when the license is about to expire.
// This happens independently from any watch triggered reconcile request.
func (r *ReconcileLicenses) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	defer common.LogReconciliationRun(log, request, "es_name", &r.iteration)()
	results := r.reconcileInternal(request)
	current, err := results.Aggregate()
	log.V(1).Info("Reconcile result", "requeue", current.Requeue, "requeueAfter", current.RequeueAfter)
	return current, err
}

// Add creates a new EnterpriseLicense Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, p operator.Parameters) error {
	return add(mgr, newReconciler(mgr, p))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, params operator.Parameters) *ReconcileLicenses {
	c := k8s.WrapClient(mgr.GetClient())
	return &ReconcileLicenses{
		Client:  c,
		scheme:  mgr.GetScheme(),
		checker: license.NewLicenseChecker(c, params.OperatorNamespace),
	}
}

func nextReconcile(expiry time.Time, safety time.Duration) reconcile.Result {
	return nextReconcileRelativeTo(time.Now(), expiry, safety)
}

func nextReconcileRelativeTo(now, expiry time.Time, safety time.Duration) reconcile.Result {
	// short-circuit to default if no expiry given
	if expiry.IsZero() {
		return reconcile.Result{
			RequeueAfter: minimumRetryInterval,
		}
	}
	requeueAfter := expiry.Add(-1 * (safety / 2)).Sub(now)
	if requeueAfter <= 0 {
		return reconcile.Result{Requeue: true}
	}
	return reconcile.Result{
		// requeue at expiry minus safetyMargin/2 to ensure we actually reissue a license on the next attempt
		RequeueAfter: requeueAfter,
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r *ReconcileLicenses) error {
	// Create a new controller
	c, err := controller.New(name, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to Elasticsearch clusters.
	if err := c.Watch(
		&source.Kind{Type: &esv1.Elasticsearch{}}, &handler.EnqueueRequestForObject{},
	); err != nil {
		return err
	}

	if err := c.Watch(&source.Kind{Type: &corev1.Secret{}}, &handler.EnqueueRequestsFromMapFunc{
		ToRequests: handler.ToRequestsFunc(func(object handler.MapObject) []reconcile.Request {
			secret, ok := object.Object.(*corev1.Secret)
			if !ok {
				log.Error(
					fmt.Errorf("unexpected object type %T in watch handler, expected Secret", object.Object),
					"dropping watch event due to error in handler")
				return nil
			}
			if !license.IsOperatorLicense(*secret) {
				return nil
			}

			// if a license is added/modified we want to update for potentially all clusters managed by this instance
			// of ECK which is why we are listing all Elasticsearch clusters here and trigger a reconciliation
			rs, err := reconcileRequestsForAllClusters(r)
			if err != nil {
				// dropping the event(s) at this point
				log.Error(err, "failed to list affected clusters in enterprise license watch")
				return nil
			}
			return rs
		}),
	}); err != nil {
		return err
	}
	return nil
}

var _ reconcile.Reconciler = &ReconcileLicenses{}

// ReconcileLicenses reconciles EnterpriseLicenses with existing Elasticsearch clusters and creates ClusterLicenses for them.
type ReconcileLicenses struct {
	k8s.Client
	scheme *runtime.Scheme
	// iteration is the number of times this controller has run its Reconcile method
	iteration uint64
	checker   license.Checker
}

// findLicense tries to find the best Elastic stack license available.
func findLicense(c k8s.Client, checker license.Checker, minVersion *version.Version) (esclient.License, string, bool) {
	licenseList, errs := license.EnterpriseLicensesOrErrors(c)
	if len(errs) > 0 {
		log.Info("Ignoring invalid license objects", "errors", errs)
	}
	return license.BestMatch(minVersion, licenseList, checker.Valid)
}

// reconcileSecret upserts a secret in the namespace of the Elasticsearch cluster containing the signature of its license.
func reconcileSecret(
	c k8s.Client,
	cluster esv1.Elasticsearch,
	parent string,
	esLicense esclient.License,
) error {
	secretName := esv1.LicenseSecretName(cluster.Name)

	licenseBytes, err := json.Marshal(esLicense)
	if err != nil {
		return err
	}

	expected := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				common.TypeLabelName:      license.Type,
				license.LicenseLabelName:  parent,
				license.LicenseLabelScope: string(license.LicenseScopeElasticsearch),
				license.LicenseLabelType:  esLicense.Type,
			},
		},
		Data: map[string][]byte{
			license.FileName: licenseBytes,
		},
	}
	// create/update a secret in the cluster's namespace containing the same data
	var reconciled corev1.Secret
	err = reconciler.ReconcileResource(reconciler.Params{
		Client:     c,
		Scheme:     scheme.Scheme,
		Owner:      &cluster,
		Expected:   &expected,
		Reconciled: &reconciled,
		NeedsUpdate: func() bool {
			return !(reflect.DeepEqual(reconciled.Data, expected.Data) &&
				compare.LabelsAndAnnotationsAreEqual(reconciled.ObjectMeta, expected.ObjectMeta))
		},
		UpdateReconciled: func() {
			reconciled.Labels = maps.Merge(reconciled.Labels, expected.Labels)
			reconciled.Data = expected.Data
		},
	})
	return err
}

// reconcileClusterLicense upserts a cluster license in the namespace of the given Elasticsearch cluster.
// Returns time to next reconciliation, bool whether a license is configured at all and optional error.
func (r *ReconcileLicenses) reconcileClusterLicense(cluster esv1.Elasticsearch) (time.Time, bool, error) {
	var noResult time.Time
	minVersion, err := r.minVersion(cluster)
	if err != nil {
		return noResult, true, err
	}
	matchingSpec, parent, found := findLicense(r, r.checker, minVersion)
	if !found {
		// no license, delete cluster level licenses to revert to basic
		log.V(1).Info("No enterprise license found. Attempting to remove cluster license secret", "namespace", cluster.Namespace, "es_name", cluster.Name)
		secretName := esv1.LicenseSecretName(cluster.Name)
		err := r.Client.Delete(&corev1.Secret{
			ObjectMeta: k8s.ToObjectMeta(types.NamespacedName{
				Namespace: cluster.Namespace,
				Name:      secretName,
			}),
		})
		if err != nil && !errors.IsNotFound(err) {
			log.Error(err, "failed to delete cluster license secret", "secret_name", secretName, "namespace", cluster.Namespace, "es_name", cluster.Name)
		}
		return noResult, true, nil

	}
	log.V(1).Info("Found license for cluster", "eck_license", parent, "es_license", matchingSpec.UID, "license_type", matchingSpec.Type, "namespace", cluster.Namespace, "es_name", cluster.Name)
	// make sure the signature secret is created in the cluster's namespace
	if err := reconcileSecret(r, cluster, parent, matchingSpec); err != nil {
		return noResult, false, err
	}
	return matchingSpec.ExpiryTime(), false, nil
}

func (r *ReconcileLicenses) minVersion(cluster esv1.Elasticsearch) (*version.Version, error) {
	pods, err := sset.GetActualPodsForCluster(r, cluster)
	if err != nil {
		return nil, err
	}
	minVersion, err := label.MinVersion(pods)
	if err != nil {
		return nil, err
	}
	if minVersion == nil {
		minVersion, err = version.Parse(cluster.Spec.Version)
		if err != nil {
			return nil, err
		}
	}
	return minVersion, nil
}

func (r *ReconcileLicenses) reconcileInternal(request reconcile.Request) *reconciler.Results {
	res := &reconciler.Results{}

	// Fetch the cluster to ensure it still exists
	cluster := esv1.Elasticsearch{}
	err := r.Get(request.NamespacedName, &cluster)
	if err != nil {
		if errors.IsNotFound(err) {
			// nothing to do no cluster
			return res
		}
		return res.WithError(err)
	}

	if !cluster.DeletionTimestamp.IsZero() {
		// cluster is being deleted nothing to do
		return res
	}

	newExpiry, noLicense, err := r.reconcileClusterLicense(cluster)
	if err != nil {
		return res.WithError(err)
	}
	margin := defaultSafetyMargin
	if noLicense {
		// don't apply safety margin if we don't have a license but use requested requeue time as specified in newExpiry
		margin = 0
	}
	return res.WithResult(nextReconcile(newExpiry, margin))
}
