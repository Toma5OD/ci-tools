package utils

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	coreapi "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	imagev1 "github.com/openshift/api/image/v1"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/kubernetes"
	"github.com/openshift/ci-tools/pkg/util"
)

func ImageDigestFor(client ctrlruntimeclient.Client, namespace func() string, name, tag string) func() (string, error) {
	return func() (string, error) {
		is := &imagev1.ImageStream{}
		if err := client.Get(context.TODO(), ctrlruntimeclient.ObjectKey{Namespace: namespace(), Name: name}, is); err != nil {
			return "", fmt.Errorf("could not retrieve output imagestream: %w", err)
		}
		var registry string
		if len(is.Status.PublicDockerImageRepository) > 0 {
			registry = is.Status.PublicDockerImageRepository
		} else if len(is.Status.DockerImageRepository) > 0 {
			registry = is.Status.DockerImageRepository
		} else {
			return "", fmt.Errorf("image stream %s has no accessible image registry value", name)
		}
		ref, image := FindStatusTag(is, tag)
		if len(image) > 0 {
			return fmt.Sprintf("%s@%s", registry, image), nil
		}
		if ref == nil && findSpecTag(is, tag) == nil {
			return "", fmt.Errorf("image stream %q has no tag %q in spec or status", name, tag)
		}
		return fmt.Sprintf("%s:%s", registry, tag), nil
	}
}

func findSpecTag(is *imagev1.ImageStream, tag string) *coreapi.ObjectReference {
	for _, t := range is.Spec.Tags {
		if t.Name != tag {
			continue
		}
		return t.From
	}
	return nil
}

// FindStatusTag returns an object reference to a tag if
// it exists in the ImageStream's Spec
func FindStatusTag(is *imagev1.ImageStream, tag string) (*coreapi.ObjectReference, string) {
	for _, t := range is.Status.Tags {
		if t.Tag != tag {
			continue
		}
		if len(t.Items) == 0 {
			return nil, ""
		}
		if len(t.Items[0].Image) == 0 {
			return &coreapi.ObjectReference{
				Kind: "DockerImage",
				Name: t.Items[0].DockerImageReference,
			}, ""
		}
		return &coreapi.ObjectReference{
			Kind:      "ImageStreamImage",
			Namespace: is.Namespace,
			Name:      fmt.Sprintf("%s@%s", is.Name, t.Items[0].Image),
		}, t.Items[0].Image
	}
	return nil, ""
}

const DefaultImageImportTimeout = 45 * time.Minute

func getEvaluator(ctx context.Context, client ctrlruntimeclient.Client, ns, name string, tags sets.Set[string]) func(obj runtime.Object) (bool, error) {
	return func(obj runtime.Object) (bool, error) {
		switch stream := obj.(type) {
		case *imagev1.ImageStream:
			for i, tag := range stream.Spec.Tags {
				if tags.Len() > 0 && !tags.Has(tag.Name) {
					continue
				}
				_, exist, condition := util.ResolvePullSpec(stream, tag.Name, true)
				if !exist {
					logrus.WithField("conditionMessage", condition.Message).Debugf("Waiting to import tag[%d] on imagestream %s/%s:%s ...", i, stream.Namespace, stream.Name, tag.Name)
					if strings.Contains(condition.Message, "Internal error occurred") {
						if err := reimportTag(ctx, client, ns, name, tag.Name); err != nil {
							return false, fmt.Errorf("failed to reimport the tag %s/%s@%s: %w", stream.Namespace, stream.Name, tag.Name, err)
						}
					}
					return false, nil
				}
			}
			return true, nil
		default:
			return false, fmt.Errorf("imagestream %s/%s got an event that did not contain an imagestream: %v", ns, name, obj)
		}
	}
}

// WaitForImportingISTag waits for the tags on the image stream are imported
func WaitForImportingISTag(ctx context.Context, client ctrlruntimeclient.WithWatch, ns, name string, into *imagev1.ImageStream, tags sets.Set[string], timeout time.Duration) error {
	obj := into
	if obj == nil {
		obj = &imagev1.ImageStream{}
	}
	return kubernetes.WaitForConditionOnObject(ctx, client, ctrlruntimeclient.ObjectKey{Namespace: ns, Name: name}, &imagev1.ImageStreamList{}, obj, getEvaluator(ctx, client, ns, name, tags), timeout)
}

func reimportTag(ctx context.Context, client ctrlruntimeclient.Client, ns, name, tag string) error {
	step := 0
	if err := wait.ExponentialBackoff(wait.Backoff{Steps: 3, Duration: 1 * time.Second, Factor: 2}, func() (bool, error) {
		logrus.Debugf("Retrying (%d) importing tag %s/%s@%s", step, ns, name, tag)
		streamImport := &imagev1.ImageStreamImport{
			ObjectMeta: meta.ObjectMeta{
				Namespace: ns,
				Name:      fmt.Sprintf("%s-%s-%d", name, tag, step),
			},
			Spec: imagev1.ImageStreamImportSpec{
				Import: true,
				Images: []imagev1.ImageImportSpec{
					{
						To: &coreapi.LocalObjectReference{
							Name: tag,
						},
						From: coreapi.ObjectReference{
							Kind: "DockerImage",
							Name: api.QuayImageReference(api.ImageStreamTagReference{Namespace: ns, Name: name, Tag: tag}),
						},
						ImportPolicy:    imagev1.TagImportPolicy{ImportMode: imagev1.ImportModePreserveOriginal},
						ReferencePolicy: imagev1.TagReferencePolicy{Type: imagev1.LocalTagReferencePolicy},
					},
				},
			},
		}
		step = step + 1
		if err := client.Create(ctx, streamImport); err != nil {
			if kerrors.IsConflict(err) {
				return false, nil
			}
			if kerrors.IsForbidden(err) {
				logrus.Warnf("Unable to lock %s/%s@%s to an image digest pull spec, you don't have permission to access the necessary API.",
					ns, name, tag)
				return false, nil
			}
			return false, err
		}
		if len(streamImport.Status.Images) == 0 {
			return false, nil
		}
		image := streamImport.Status.Images[0]
		if image.Image == nil {
			return false, nil
		}
		logrus.Debugf("Imported tag %s/%s@%s", ns, name, tag)
		return true, nil
	}); err != nil {
		if err == wait.ErrorInterrupted(err) {
			return fmt.Errorf("unable to import tag %s/%s@%s even with (%d) imports: %w", ns, name, tag, step, err)
		}
		return fmt.Errorf("unable to import tag %s/%s@%s at import (%d): %w", ns, name, tag, step-1, err)
	}
	return nil
}
