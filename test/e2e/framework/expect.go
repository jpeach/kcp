/*
Copyright 2021 The KCP Authors.

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

package framework

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/cache"

	tenancyv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/tenancy/v1alpha1"
	kcpclientset "github.com/kcp-dev/kcp/pkg/client/clientset/versioned"
	kcpexternalversions "github.com/kcp-dev/kcp/pkg/client/informers/externalversions"
)

// Expectation closes over a statement of intent, allowing the caller
// to accumulate errors and determine when the expectation should cease
// to be evaluated.
type Expectation func(ctx context.Context) (done bool, err error)

// Expecter allows callers to register expectations
type Expecter interface {
	// ExpectBefore will result in the Expectation being evaluated whenever
	// state changes, up until the desired timeout is reached.
	ExpectBefore(context.Context, Expectation, time.Duration)
}

// NewExpecter creates a informer-driven registry of expectations, which will
// be triggered on every event that the informer ingests.
func NewExpecter(informer cache.SharedIndexInformer) *expectationController {
	controller := expectationController{
		informer:     informer,
		expectations: map[uuid.UUID]func(){},
		lock:         sync.RWMutex{},
	}

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(_ interface{}) {
			controller.triggerExpectations()
		},
		UpdateFunc: func(_, _ interface{}) {
			controller.triggerExpectations()
		},
		DeleteFunc: func(_ interface{}) {
			controller.triggerExpectations()
		},
	})

	return &controller
}

// expectationController triggers the registered expectations on informer events
type expectationController struct {
	informer cache.SharedIndexInformer
	// expectations are recorded by UUID so they may be removed after they complete
	expectations map[uuid.UUID]func()
	lock         sync.RWMutex
}

func (c *expectationController) triggerExpectations() {
	c.lock.RLock()
	defer c.lock.RUnlock()

	for _, expectation := range c.expectations {
		expectation()
	}
}

func (c *expectationController) ExpectBefore(ctx context.Context, expectation Expectation, duration time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	type result struct {
		done bool
		err  error
	}
	results := make(chan result)

	// producer wraps the expectation and allows the informer-driven flow to trigger
	// it while the side effects of the call feed the channel we listen to here.
	expectationCtx, expectationCancel := context.WithCancel(ctx)
	defer expectationCancel()
	producer := func() {
		done, err := expectation(expectationCtx)
		if expectationCtx.Err() == nil {
			results <- result{
				done: done,
				err:  err,
			}
		}
	}

	id := uuid.New()
	c.lock.Lock()
	c.expectations[id] = producer
	c.lock.Unlock()

	defer func() {
		c.lock.Lock()
		delete(c.expectations, id)
		c.lock.Unlock()
	}()

	// evaluate once to get the current state once we're registered to see future events
	go producer()

	var expectationErrors []error
	var processed int
	for {
		select {
		case <-ctx.Done():
			expectationCancel()
			close(results)
			return fmt.Errorf("expected state not found: %w, %d errors encountered while processing %d events: %v", ctx.Err(), len(expectationErrors), processed, kerrors.NewAggregate(expectationErrors))
		case result := <-results:
			processed += 1
			if result.err != nil {
				expectationErrors = append(expectationErrors, result.err)
			}
			if result.done {
				if result.err == nil {
					return nil
				}
				return kerrors.NewAggregate(expectationErrors)
			}
		}
	}
}

// The following are statically-typed helpers for common types to allow us to express expectations about objects.

// RegisterWorkspaceExpectation registers an expectation about the future state of the seed.
type RegisterWorkspaceExpectation func(seed *tenancyv1alpha1.Workspace, expectation WorkspaceExpectation) error

// WorkspaceExpectation evaluates an expectation about the object.
type WorkspaceExpectation func(*tenancyv1alpha1.Workspace) error

// ExpectWorkspaces sets up an Expecter in order to allow registering expectations in tests with minimal setup.
func ExpectWorkspaces(ctx context.Context, t TestingTInterface, client kcpclientset.Interface) (RegisterWorkspaceExpectation, error) {
	kcpSharedInformerFactory := kcpexternalversions.NewSharedInformerFactoryWithOptions(client, 0)
	workspaceInformer := kcpSharedInformerFactory.Tenancy().V1alpha1().Workspaces()
	expecter := NewExpecter(workspaceInformer.Informer())
	kcpSharedInformerFactory.Start(ctx.Done())
	if !cache.WaitForNamedCacheSync(t.Name(), ctx.Done(), workspaceInformer.Informer().HasSynced) {
		return nil, errors.New("failed to wait for caches to sync")
	}
	return func(seed *tenancyv1alpha1.Workspace, expectation WorkspaceExpectation) error {
		key, err := cache.MetaNamespaceKeyFunc(seed)
		if err != nil {
			return err
		}
		return expecter.ExpectBefore(ctx, func(ctx context.Context) (done bool, err error) {
			current, err := workspaceInformer.Lister().Get(key)
			if err != nil {
				return !apierrors.IsNotFound(err), err
			}
			expectErr := expectation(current.DeepCopy())
			return expectErr == nil, expectErr
		}, 30*time.Second)
	}, nil
}