// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package inputisolation

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (s *Service) describeRejections(ctx context.Context, groups []changeGroup, candidate, base map[string]*k8sstore.SnapshotBranch) []Rejection {
	var rejected []Rejection
	for _, group := range groups {
		for _, alias := range slices.Sorted(maps.Keys(group.changes)) {
			for _, change := range group.changes[alias] {
				rejection := Rejection{Store: alias, Namespace: change.Namespace(), Name: change.Name(), Deleted: change.Deleted(), Reason: templating.RenderErrorMessage(group.reason), revision: change.Revision()}
				branch := candidate[alias]
				if change.Deleted() {
					branch = base[alias]
				}
				ref, err := rejectionReference(ctx, branch, &rejection)
				if err != nil {
					s.logger.Warn("Cannot identify rejected resource for a Kubernetes Event", "store", alias, "namespace", rejection.Namespace, "name", rejection.Name, "error", err)
				}
				rejection.Object = ref
				rejected = append(rejected, rejection)
			}
		}
	}
	return rejected
}

func rejectionReference(ctx context.Context, branch *k8sstore.SnapshotBranch, rejection *Rejection) (corev1.ObjectReference, error) {
	value, found, err := branch.GetIdentityContext(ctx, rejection.Namespace, rejection.Name)
	if err != nil {
		return corev1.ObjectReference{}, err
	}
	if !found {
		return corev1.ObjectReference{}, fmt.Errorf("rejected revision is unavailable")
	}
	if object, ok := value.(map[string]any); ok {
		value = &unstructured.Unstructured{Object: object}
	}
	metadata, err := meta.Accessor(value)
	if err != nil {
		return corev1.ObjectReference{}, err
	}
	typeMeta, err := meta.TypeAccessor(value)
	if err != nil {
		return corev1.ObjectReference{}, err
	}
	return corev1.ObjectReference{
		APIVersion: typeMeta.GetAPIVersion(), Kind: typeMeta.GetKind(),
		Namespace: metadata.GetNamespace(), Name: metadata.GetName(), UID: metadata.GetUID(), ResourceVersion: metadata.GetResourceVersion(),
	}, nil
}

func (s *Service) reportRejections(previous *acceptedInputs, rejected []Rejection) {
	seen := map[string]struct{}{}
	retained := map[rejectedRevision]struct{}{}
	if previous != nil {
		for i := range previous.rejections {
			retained[rejectionRevision(&previous.rejections[i])] = struct{}{}
		}
	}
	for i := range rejected {
		rejection := &rejected[i]
		if _, unchanged := retained[rejectionRevision(rejection)]; unchanged {
			continue
		}

		watch := s.watches[rejection.Store]
		group, _, qualified := strings.Cut(watch.APIVersion, "/")
		if !qualified {
			group = ""
		}
		identity := group + "/" + watch.Resources + "/" + rejection.Namespace + "/" + rejection.Name
		if _, reported := seen[identity]; reported {
			continue
		}
		seen[identity] = struct{}{}
		s.logger.Warn("Watched resource change rejected; other resources continue updating. Correct this resource to apply the change.",
			"store", rejection.Store, "namespace", rejection.Namespace, "name", rejection.Name, "deleted", rejection.Deleted, "reason", rejection.Reason, "diagnostics", "/debug/vars/inputRejections")
	}
}

type rejectedRevision struct {
	store, namespace, name string
	revision               stores.Revision
	deleted                bool
}

func rejectionRevision(rejection *Rejection) rejectedRevision {
	return rejectedRevision{rejection.Store, rejection.Namespace, rejection.Name, rejection.revision, rejection.Deleted}
}
