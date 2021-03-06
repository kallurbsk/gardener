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

package shoot

import (
	"context"
	"fmt"
	"sync/atomic"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	"github.com/gardener/gardener/pkg/client/kubernetes/clientmap"
	"github.com/gardener/gardener/pkg/client/kubernetes/clientmap/keys"
	"github.com/gardener/gardener/pkg/controllerutils"
	gardenlogger "github.com/gardener/gardener/pkg/logger"
	"github.com/gardener/gardener/pkg/utils"
	contextutil "github.com/gardener/gardener/pkg/utils/context"
	"github.com/gardener/gardener/pkg/utils/flow"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (c *Controller) shootReferenceAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		gardenlogger.Logger.Errorf("Couldn't get key for object %+v: %v", obj, err)
		return
	}
	c.shootReferenceQueue.Add(key)
}

func (c *Controller) shootReferenceUpdate(oldObj, newObj interface{}) {
	var (
		oldShoot = oldObj.(*gardencorev1beta1.Shoot)
		newShoot = newObj.(*gardencorev1beta1.Shoot)
	)

	if refChange(oldShoot, newShoot) || newShoot.DeletionTimestamp != nil && !controllerutils.HasFinalizer(newShoot, gardencorev1beta1.GardenerName) {
		key, err := cache.MetaNamespaceKeyFunc(newObj)
		if err != nil {
			gardenlogger.Logger.Errorf("Couldn't get key for object %+v: %v", newObj, err)
			return
		}
		c.shootReferenceQueue.Add(key)
	}
}

func refChange(oldShoot, newShoot *gardencorev1beta1.Shoot) bool {
	return !apiequality.Semantic.Equalities.DeepEqual(oldShoot.Spec.DNS, newShoot.Spec.DNS)
}

// FinalizerName is the name of the finalizer used for the reference protection.
const FinalizerName = "gardener.cloud/reference-protection"

// SecretLister fetches secret objects with the given options.
type SecretLister func(ctx context.Context, secretList *corev1.SecretList, options ...client.ListOption) error

// NewShootReferenceReconciler creates a new instance of a reconciler which checks object references from shoot objects.
// A special `userSecretLister` serves as an option to retrieve secret objects which are not gardener managed.
func NewShootReferenceReconciler(l logrus.FieldLogger, clientMap clientmap.ClientMap, userSecretLister SecretLister) reconcile.Reconciler {
	return &shootReferenceReconciler{
		clientMap:    clientMap,
		secretLister: userSecretLister,
		logger:       l,
	}
}

type shootReferenceReconciler struct {
	ctx context.Context

	// secretLister is supposed to be the most performant option to retrieve secret objects in this controller.
	secretLister SecretLister
	clientMap    clientmap.ClientMap

	logger logrus.FieldLogger
}

// InjectClient implements `inject.Stoppable`.
func (r *shootReferenceReconciler) InjectStopChannel(stopCh <-chan struct{}) error {
	r.ctx = contextutil.FromStopChannel(stopCh)
	return nil
}

// Reconcile checks the shoot in the given request for references to further objects in order to protect them from
// deletions as long as they are still referenced.
func (r *shootReferenceReconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	gardenClient, err := r.clientMap.GetClient(r.ctx, keys.ForGarden())
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get garden client: %w", err)
	}

	shoot := &gardencorev1beta1.Shoot{}
	if err := gardenClient.Client().Get(r.ctx, request.NamespacedName, shoot); err != nil {
		if apierrors.IsNotFound(err) {
			r.logger.Infof("Object %q is gone, stop reconciling: %v", request.Name, err)
			return reconcile.Result{}, nil
		}
		r.logger.Infof("Unable to retrieve object %q from store: %v", request.Name, err)
		return reconcile.Result{}, err
	}

	r.logger.Infof("[SHOOT REFERENCE CONTROL] %s", request)

	if err := r.reconcileShootReferences(gardenClient.Client(), shoot); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (r *shootReferenceReconciler) reconcileShootReferences(c client.Client, shoot *gardencorev1beta1.Shoot) error {
	// Iterate over all user secrets in project namespace and check if they can be released.
	if err := r.releaseUnreferencedSecrets(c, shoot); err != nil {
		return err
	}

	// Remove finalizer from shoot in case it's being deleted and not handled by Gardener any more.
	if shoot.DeletionTimestamp != nil && !controllerutils.HasFinalizer(shoot, gardencorev1beta1.GardenerName) {
		return controllerutils.PatchRemoveFinalizers(r.ctx, c, shoot, FinalizerName)
	}

	// Add finalizer to referenced secrets that are not managed by Gardener.
	needsFinalizer, err := r.handleReferencedSecrets(c, shoot)
	if err != nil {
		return err
	}

	// Manage finalizers on shoot.
	hasFinalizer := controllerutils.HasFinalizer(shoot, FinalizerName)
	if needsFinalizer && !hasFinalizer {
		return controllerutils.PatchFinalizers(r.ctx, c, shoot, FinalizerName)
	}
	if !needsFinalizer && hasFinalizer {
		return controllerutils.PatchRemoveFinalizers(r.ctx, c, shoot, FinalizerName)
	}
	return nil
}

func (r *shootReferenceReconciler) handleReferencedSecrets(c client.Client, shoot *gardencorev1beta1.Shoot) (bool, error) {
	var (
		fns            []flow.TaskFn
		added          = uint32(0)
		dnsSecretNames = secretNamesForDNSProviders(shoot)
	)

	for _, dnsSecretName := range dnsSecretNames {
		name := dnsSecretName
		fns = append(fns, func(ctx context.Context) error {
			secret := &corev1.Secret{}
			s := shoot
			if err := c.Get(ctx, kutil.Key(s.Namespace, name), secret); err != nil {
				return err
			}

			// Don't handle Gardener managed secrets.
			if _, ok := secret.Labels[v1beta1constants.GardenRole]; ok {
				return nil
			}

			atomic.StoreUint32(&added, 1)

			if controllerutils.HasFinalizer(secret, FinalizerName) {
				return nil
			}
			return controllerutils.PatchFinalizers(r.ctx, c, secret, FinalizerName)
		})
	}
	err := flow.Parallel(fns...)(r.ctx)

	return added != 0, err
}

func (r *shootReferenceReconciler) releaseUnreferencedSecrets(c client.Client, shoot *gardencorev1beta1.Shoot) error {
	secrets, err := r.getUnreferencedSecrets(c, shoot)
	if err != nil {
		return err
	}

	var fns []flow.TaskFn
	for _, secret := range secrets {
		s := secret
		fns = append(fns, func(ctx context.Context) error {
			return client.IgnoreNotFound(controllerutils.PatchRemoveFinalizers(r.ctx, c, &s, FinalizerName))
		})

	}
	return flow.Parallel(fns...)(r.ctx)
}

var (
	noGardenRole = utils.MustNewRequirement(v1beta1constants.GardenRole, selection.DoesNotExist)

	// UserManagedSelector is a selector for objects which are managed by users and not created by Gardener.
	UserManagedSelector = client.MatchingLabelsSelector{Selector: labels.NewSelector().Add(noGardenRole)}
)

func (r *shootReferenceReconciler) getUnreferencedSecrets(c client.Client, shoot *gardencorev1beta1.Shoot) ([]corev1.Secret, error) {
	namespace := shoot.Namespace

	secrets := &corev1.SecretList{}
	if err := r.secretLister(r.ctx, secrets, client.InNamespace(namespace), UserManagedSelector); err != nil {
		return nil, err
	}

	shoots := &gardencorev1beta1.ShootList{}
	if err := c.List(r.ctx, shoots, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	referencedSecrets := sets.NewString()
	for _, s := range shoots.Items {
		// Ignore own references if shoot is in deletion and references are not needed any more by Gardener.
		if s.Name == shoot.Name && shoot.DeletionTimestamp != nil && !controllerutils.HasFinalizer(&s, gardencorev1beta1.GardenerName) {
			continue
		}
		referencedSecrets.Insert(secretNamesForDNSProviders(&s)...)
	}

	var secretsToRelease []corev1.Secret
	for _, secret := range secrets.Items {
		if !controllerutils.HasFinalizer(&secret, FinalizerName) {
			continue
		}
		if referencedSecrets.Has(secret.Name) {
			continue
		}
		secretsToRelease = append(secretsToRelease, secret)
	}

	return secretsToRelease, nil
}

func secretNamesForDNSProviders(shoot *gardencorev1beta1.Shoot) []string {
	if shoot.Spec.DNS == nil {
		return nil
	}
	var names = make([]string, 0, len(shoot.Spec.DNS.Providers))
	for _, provider := range shoot.Spec.DNS.Providers {
		if provider.SecretName == nil {
			continue
		}
		names = append(names, *provider.SecretName)
	}

	return names
}
