// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/tests/process"
)

var mainTemplateBlock = regexp.MustCompile(`(?m)^haproxyConfig:\n(\s+)template: \|\n`)

func corruptMainTemplate(content []byte) ([]byte, error) {
	match := mainTemplateBlock.FindSubmatchIndex(content)
	if match == nil {
		return nil, errors.New("base library has no haproxyConfig.template to corrupt")
	}
	result := append([]byte{}, content[:match[1]]...)
	result = append(result, content[match[2]:match[3]]...)
	result = append(result, []byte("  "+brokenTemplate+"\n")...)
	return append(result, content[match[1]:]...), nil
}

func (u *upgrade) brokenRelease(ctx context.Context) error {
	s := u.session
	if err := os.CopyFS(u.brokenChart, os.DirFS(u.chart)); err != nil {
		return err
	}
	library := filepath.Join(u.brokenChart, "charts", "base", "library.yaml")
	chart, err := os.OpenRoot(u.brokenChart)
	if err != nil {
		return err
	}
	defer chart.Close()
	const libraryPath = "charts/base/library.yaml"
	content, err := chart.ReadFile(libraryPath)
	if err != nil {
		return err
	}
	content, err = corruptMainTemplate(content)
	if err != nil {
		return err
	}
	if err := chart.WriteFile(libraryPath, content, 0o600); err != nil {
		return err
	}
	image := s.ImageRepository + ":" + s.ImageTag + "-broken-haproxy" + s.HAProxyVersion
	args := []string{"build", "--build-arg", "BASE_IMAGE=" + s.ImageRepository + ":" + s.ImageTag, "-t", image, "-f", "-", filepath.Dir(library)}
	result, err := s.Runner.Run(ctx, &process.Command{Name: dockerCommand, Args: args, Env: s.Cluster.Environment, Stdin: strings.NewReader("ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\nCOPY library.yaml /usr/share/haptic/chart/charts/base/library.yaml\n")})
	if _, err = s.record("docker", args, result, err); err != nil {
		return err
	}
	return s.Cluster.LoadImages(ctx, image)
}

func (u *upgrade) rejectBrokenRelease(ctx context.Context) error {
	s := u.session
	hooks, err := s.Helm(ctx, "get", "hooks", s.Release)
	if err != nil {
		return err
	}
	if !strings.Contains(hooks.Stdout, "pre-rollout") {
		return errors.New("release has no pre-rollout hook; the negative control would test no gate")
	}
	if err := u.brokenRelease(ctx); err != nil {
		return err
	}
	before, err := s.ConfigSnapshot(ctx)
	if err != nil {
		return err
	}
	beforeDeployment, err := s.Deployment(ctx, controllerComponent)
	if err != nil {
		return err
	}
	args := append(u.targetArgs(u.brokenChart), "--set", "controller.image.tag="+s.ImageTag+"-broken", "--timeout", "5m")
	if _, err := s.Helm(ctx, args...); err == nil {
		return errors.New("upgrade accepted an uncompilable release")
	}
	if err := s.requireFailedPreflight(ctx); err != nil {
		return err
	}
	after, err := s.ConfigSnapshot(ctx)
	if err != nil {
		return err
	}
	if err := before.RequireUnchanged(after); err != nil {
		return err
	}
	afterDeployment, err := s.Deployment(ctx, controllerComponent)
	if err != nil {
		return err
	}
	if beforeDeployment.UID != afterDeployment.UID || !reflect.DeepEqual(beforeDeployment.Spec, afterDeployment.Spec) {
		return errors.New("rejected incoming configuration changed the controller Deployment")
	}
	if bytes.Contains(after.Content, []byte(brokenTemplate)) {
		return errors.New("broken template reached live configuration despite rejection")
	}
	if err := s.RequireNoControllerRestarts(ctx); err != nil {
		return err
	}
	if err := s.WaitControllerReady(ctx, 180*time.Second); err != nil {
		return err
	}
	return s.UpgradeTraffic(ctx, "rejected")
}

func (s *Session) requireFailedPreflight(ctx context.Context) error {
	jobs, err := readJSON[batchv1.JobList](ctx, s, "get", "jobs")
	if err != nil {
		return err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if strings.Contains(job.Name, "-pre-rollout") && job.Status.Failed >= 1 {
			if _, err := s.Kube(ctx, nil, "logs", "job/"+job.Name, "--tail=15"); err != nil {
				s.Infof("Failed preflight log unavailable: %v", err)
			}
			return nil
		}
	}
	return errors.New("no failed pre-rollout job; upgrade failed outside the preflight gate")
}

func (u *upgrade) repairBrokenDeployment(ctx context.Context) error {
	s := u.session
	deployment, err := s.Deployment(ctx, controllerComponent)
	if err != nil {
		return err
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas <= 0 {
		return errors.New("controller has no replicas to stop before staging the broken configuration")
	}
	replicas := strconv.FormatInt(int64(*deployment.Spec.Replicas), 10)
	if _, err := s.Kube(ctx, nil, "scale", "deployment/"+deployment.Name, "--replicas=0"); err != nil {
		return err
	}
	if _, err := s.Kube(ctx, nil, "wait", "--for=delete", "pod", "-l", "app.kubernetes.io/component=controller", "--timeout=120s"); err != nil {
		return err
	}
	if err := u.stageBrokenLiveConfiguration(ctx); err != nil {
		return err
	}
	staged, err := s.ConfigSnapshot(ctx)
	if err != nil {
		return err
	}
	if !bytes.Contains(staged.Content, []byte(brokenTemplate)) {
		return errors.New("broken template never reached live configuration; recovery would test no failure")
	}
	if _, err := s.Kube(ctx, nil, "scale", "deployment/"+deployment.Name, "--replicas="+replicas); err != nil {
		return err
	}
	if err := s.WaitControllerReady(ctx, 150*time.Second); err == nil {
		return errors.New("deliberately broken controller became Ready; recovery would test no failure")
	} else if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return fmt.Errorf("observe broken controller: %w", err)
	}
	if _, err := s.Helm(ctx, append(u.targetArgs(u.chart), "--timeout", "20m")...); err != nil {
		return err
	}
	if err := s.ControllerRollout(ctx); err != nil {
		return err
	}
	if err := s.WaitValidated(ctx, 180*time.Second); err != nil {
		return err
	}
	return s.UpgradeTraffic(ctx, "recovered")
}

func (u *upgrade) stageBrokenLiveConfiguration(ctx context.Context) error {
	s := u.session
	if u.baseline != handoverBaseline {
		_, err := s.Helm(ctx, append(u.targetArgs(u.brokenChart), "--set", "controller.replicaCount=0", "--timeout", "10m")...)
		return err
	}
	libraries, err := readJSON[unstructured.UnstructuredList](ctx, s, "get", "haproxytemplatelibraries")
	if err != nil {
		return err
	}
	name := ""
	for _, library := range libraries.Items {
		_, found, err := unstructured.NestedString(library.Object, "spec", "haproxyConfig", "template")
		if err != nil {
			return err
		}
		if found {
			if name != "" {
				return errors.New("more than one library supplies the main template")
			}
			name = library.GetName()
		}
	}
	if name == "" {
		return errors.New("released main-template library is missing")
	}
	patch, err := json.Marshal([]jsonPatchOp{{"replace", "/spec/haproxyConfig/template", brokenTemplate}})
	if err != nil {
		return err
	}
	if _, err := s.Kube(ctx, nil, "patch", "haproxytemplatelibrary", name, "--type=json", "--field-manager="+stagingManager, "-p", string(patch)); err != nil {
		return err
	}
	return u.releaseStagingOwnership(ctx, name)
}

// stagingManager owns the injected breakage only until releaseStagingOwnership
// drops it; a retained owner would make the repair's server-side apply conflict.
const stagingManager = "upgrade-scenario-staging"

type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

func (u *upgrade) releaseStagingOwnership(ctx context.Context, name string) error {
	library, err := readJSON[unstructured.Unstructured](ctx, u.session, "get", "haproxytemplatelibrary", name, "--show-managed-fields")
	if err != nil {
		return err
	}
	entries := library.GetManagedFields()
	kept := slices.DeleteFunc(slices.Clone(entries), func(entry metav1.ManagedFieldsEntry) bool { return entry.Manager == stagingManager })
	if len(kept) == len(entries) {
		return fmt.Errorf("library %s has no %s ownership to release", name, stagingManager)
	}
	patch, err := json.Marshal([]jsonPatchOp{
		{"test", "/metadata/resourceVersion", library.GetResourceVersion()},
		{"replace", "/metadata/managedFields", kept},
	})
	if err != nil {
		return err
	}
	_, err = u.session.Kube(ctx, nil, "patch", "haproxytemplatelibrary", name, "--type=json", "--field-manager="+stagingManager, "-p", string(patch))
	return err
}
