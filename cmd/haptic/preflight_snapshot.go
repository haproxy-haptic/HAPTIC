package main

import (
	"bytes"
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/conversion"
)

type liveObject struct {
	gvr    schema.GroupVersionResource
	object *unstructured.Unstructured
}
type missingObject struct {
	gvr             schema.GroupVersionResource
	namespace, name string
}
type liveConfiguration struct {
	objects []liveObject
	missing []missingObject
}

func readLiveConfiguration(ctx context.Context, cluster *liveCluster, namespace, name string) (*liveConfiguration, error) {
	snapshot := &liveConfiguration{}
	config, err := snapshot.read(ctx, cluster, templateConfigGVR, namespace, name)
	if apierrors.IsNotFound(err) {
		err = snapshot.requireUnchanged(ctx, cluster)
		return snapshot, err
	}
	if err != nil {
		return nil, err
	}
	refs, err := conversion.LibraryRefsOf(config)
	if err != nil {
		return nil, fmt.Errorf("read existing library references: %w", err)
	}
	for _, ref := range refs {
		library, err := snapshot.read(ctx, cluster, templateLibraryGVR, namespace, ref.Name)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if conversion.RevisionOf(library) != ref.Revision {
			return nil, fmt.Errorf("existing library %s/%s has a different revision from its config; finish the current apply before upgrading", namespace, ref.Name)
		}
	}
	if err := snapshot.requireUnchanged(ctx, cluster); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *liveConfiguration) read(ctx context.Context, cluster *liveCluster, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	object, err := cluster.dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		s.missing = append(s.missing, missingObject{gvr, namespace, name})
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("read existing %s %s/%s: %w", gvr.Resource, namespace, name, err)
	}
	s.objects = append(s.objects, liveObject{gvr, object})
	return object, nil
}

func (s *liveConfiguration) requireUnchanged(ctx context.Context, cluster *liveCluster) error {
	for _, entry := range s.objects {
		old := entry.object
		current, err := cluster.dyn.Resource(entry.gvr).Namespace(old.GetNamespace()).Get(ctx, old.GetName(), metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("recheck existing configuration: %w", err)
		}
		if current.GetUID() != old.GetUID() || current.GetGeneration() != old.GetGeneration() {
			return fmt.Errorf("existing %s %s/%s changed during preparation; retry against the current configuration", entry.gvr.Resource, old.GetNamespace(), old.GetName())
		}
	}
	for _, missing := range s.missing {
		_, err := cluster.dyn.Resource(missing.gvr).Namespace(missing.namespace).Get(ctx, missing.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("recheck missing configuration: %w", err)
		}
		return fmt.Errorf("existing %s %s/%s appeared during preparation; retry against the current configuration", missing.gvr.Resource, missing.namespace, missing.name)
	}
	return nil
}

func (s *liveConfiguration) documents() ([]byte, error) {
	var result bytes.Buffer
	for _, entry := range s.objects {
		data, err := yaml.Marshal(entry.object.Object)
		if err != nil {
			return nil, err
		}
		result.WriteString("---\n")
		result.Write(data)
	}
	return result.Bytes(), nil
}

func (s *liveConfiguration) validateAssembly() error {
	documents := make([]*unstructured.Unstructured, 0, len(s.objects))
	for _, entry := range s.objects {
		documents = append(documents, entry.object)
	}
	sources, err := conversion.AssembleSources(documents)
	if err != nil {
		return err
	}
	_, _, err = conversion.MergeSpecs(sources)
	return err
}
