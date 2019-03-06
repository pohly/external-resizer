/*
Copyright 2019 The Kubernetes Authors.

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

package resizer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/connection"
	csirpc "github.com/kubernetes-csi/csi-lib-utils/rpc"
	"github.com/kubernetes-csi/external-resizer/pkg/client"

	"google.golang.org/grpc"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	storagev1listers "k8s.io/client-go/listers/storage/v1"
	"k8s.io/klog"
)

const (
	resizerSecretNameKey      = "csi.storage.k8s.io/resizer-secret-name"
	resizerSecretNamespaceKey = "csi.storage.k8s.io/resizer-secret-namespace"
)

// NewCSIResizer creates a new resizer responsible for resizing CSI volumes.
func NewCSIResizer(
	address string, timeout time.Duration,
	k8sClient kubernetes.Interface, informerFactory informers.SharedInformerFactory) (Resizer, error) {

	conn, err := connection.Connect(address)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to CSI driver: %v", err)
	}

	err = csirpc.ProbeForever(conn, timeout)
	if err != nil {
		return nil, fmt.Errorf("failed probing CSI driver: %v", err)
	}

	name, err := csirpc.GetDriverName(context.Background(), conn)
	if err != nil {
		return nil, fmt.Errorf("get driver name failed: %v", err)
	}

	supports, err := supportsPluginControllerService(conn, timeout)
	if err != nil {
		return nil, fmt.Errorf("failed to check if plugin supports controller service: %v", err)
	}

	if !supports {
		return nil, errors.New("CSI driver does not support controller service")
	}

	supports, err = supportsControllerResize(conn, timeout)
	if err != nil {
		return nil, fmt.Errorf("failed to check if plugin supports controller resize: %v", err)
	}

	if !supports {
		return nil, fmt.Errorf("CSI driver does not support controller resize")
	}

	return &csiResizer{
		name:    name,
		client:  client.New(conn),
		timeout: timeout,

		k8sClient: k8sClient,
		scLister:  informerFactory.Storage().V1().StorageClasses().Lister(),
	}, nil
}

type csiResizer struct {
	name    string
	client  client.Client
	timeout time.Duration

	k8sClient kubernetes.Interface
	scLister  storagev1listers.StorageClassLister
}

func (r *csiResizer) Name() string {
	return r.name
}

func (r *csiResizer) CanSupport(pv *v1.PersistentVolume) bool {
	source := pv.Spec.CSI
	if source == nil {
		klog.V(4).Infof("PV %s is not a CSI volume, skip it", pv.Name)
		return false
	}
	if source.Driver != r.name {
		klog.V(4).Infof("Skip resize PV %s for resizer %s", pv.Name, source.Driver)
		return false
	}
	return true
}

func (r *csiResizer) Resize(pv *v1.PersistentVolume, requestSize resource.Quantity) (resource.Quantity, bool, error) {
	oldSize := pv.Spec.Capacity[v1.ResourceStorage]

	source := pv.Spec.CSI
	if source == nil {
		return oldSize, false, errors.New("not a CSI volume")
	}
	volumeID := source.VolumeHandle
	if len(volumeID) == 0 {
		return oldSize, false, errors.New("empty volume handle")
	}

	var secrets map[string]string
	// Get expand secrets from StorageClass parameters.
	scName := pv.Spec.StorageClassName
	if len(scName) > 0 {
		storageClass, err := r.scLister.Get(scName)
		if err != nil {
			return oldSize, false, fmt.Errorf("get StorageClass %s failed: %v", scName, err)
		}
		expandSecretRef, err := getSecretReference(storageClass.Parameters, pv.Name)
		if err != nil {
			return oldSize, false, err
		}
		secrets, err = getCredentials(r.k8sClient, expandSecretRef)
		if err != nil {
			return oldSize, false, err
		}
	}

	ctx, cancel := timeoutCtx(r.timeout)
	defer cancel()
	newSizeBytes, nodeResizeRequired, err := r.client.Expand(ctx, volumeID, requestSize.Value(), secrets)
	if err != nil {
		return oldSize, nodeResizeRequired, err
	}
	return *resource.NewQuantity(newSizeBytes, resource.BinarySI), nodeResizeRequired, err
}

func supportsPluginControllerService(conn *grpc.ClientConn, timeout time.Duration) (bool, error) {
	ctx, cancel := timeoutCtx(timeout)
	defer cancel()

	caps, err := csirpc.GetPluginCapabilities(ctx, conn)
	if err != nil {
		return false, fmt.Errorf("error getting controller capabilities: %v", err)
	}

	return caps[csi.PluginCapability_Service_CONTROLLER_SERVICE], nil
}

func supportsControllerResize(conn *grpc.ClientConn, timeout time.Duration) (bool, error) {
	ctx, cancel := timeoutCtx(timeout)
	defer cancel()

	caps, err := csirpc.GetControllerCapabilities(ctx, conn)
	if err != nil {
		return false, fmt.Errorf("error getting controller capabilities: %v", err)
	}

	return caps[csi.ControllerServiceCapability_RPC_EXPAND_VOLUME], nil
}

func timeoutCtx(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

// verifyAndGetSecretNameAndNamespaceTemplate gets the values (templates) associated
// with the parameters specified in "secret" and verifies that they are specified correctly.
func verifyAndGetSecretNameAndNamespaceTemplate(storageClassParams map[string]string) (string, string, error) {
	nameTemplate := storageClassParams[resizerSecretNameKey]
	namespaceTemplate := storageClassParams[resizerSecretNamespaceKey]

	// Name and namespaces are both specified.
	if nameTemplate != "" && namespaceTemplate != "" {
		return nameTemplate, namespaceTemplate, nil
	}

	// No secrets specified
	if nameTemplate == "" && namespaceTemplate == "" {
		return "", "", nil
	}

	// Only one of the names and namespaces is set.
	return "", "", errors.New("resizer secrets specified in parameters but value of either namespace or name is empty")
}

// getSecretReference returns a reference to the secret specified in the given nameTemplate
//  and namespaceTemplate, or an error if the templates are not specified correctly.
// no lookup of the referenced secret is performed, and the secret may or may not exist.
//
// supported tokens for name resolution:
// - ${pv.name}
// - ${pvc.namespace}
// - ${pvc.name}
// - ${pvc.annotations['ANNOTATION_KEY']} (e.g. ${pvc.annotations['example.com/node-publish-secret-name']})
//
// supported tokens for namespace resolution:
// - ${pv.name}
// - ${pvc.namespace}
//
// an error is returned in the following situations:
// - the nameTemplate or namespaceTemplate contains a token that cannot be resolved
// - the resolved name is not a valid secret name
// - the resolved namespace is not a valid namespace name
func getSecretReference(storageClassParams map[string]string, pvName string) (*v1.SecretReference, error) {
	nameTemplate, namespaceTemplate, err := verifyAndGetSecretNameAndNamespaceTemplate(storageClassParams)
	if err != nil {
		return nil, fmt.Errorf("failed to get name and namespace template from params: %v", err)
	}
	if nameTemplate == "" && namespaceTemplate == "" {
		return nil, nil
	}

	// Secret name and namespace template can make use of the PV name.
	// Note that neither of those things are under the control of the user.
	params := map[string]string{"pv.name": pvName}
	resolvedNamespace, err := resolveTemplate("namespace", namespaceTemplate, params)
	if err != nil {
		return nil, fmt.Errorf("error resolving secret namespace %q: %v", namespaceTemplate, err)
	}
	resolvedName, err := resolveTemplate("name", nameTemplate, params)
	if err != nil {
		return nil, fmt.Errorf("error resolving value %q: %v", nameTemplate, err)
	}

	return &v1.SecretReference{Name: resolvedName, Namespace: resolvedNamespace}, nil
}

func resolveTemplate(field, template string, params map[string]string) (string, error) {
	missingParams := sets.NewString()
	resolved := os.Expand(template, func(k string) string {
		v, ok := params[k]
		if !ok {
			missingParams.Insert(k)
		}
		return v
	})
	if missingParams.Len() > 0 {
		return "", fmt.Errorf("invalid tokens: %q", missingParams.List())
	}
	if len(validation.IsDNS1123Label(resolved)) > 0 {
		if template != resolved {
			return "", fmt.Errorf("%q resolved to %q which is not a valid %s name", template, resolved, field)
		}
		return "", fmt.Errorf("%q is not a valid %s name", template, field)
	}
	return resolved, nil
}

func getCredentials(k8sClient kubernetes.Interface, ref *v1.SecretReference) (map[string]string, error) {
	if ref == nil {
		return nil, nil
	}

	secret, err := k8sClient.CoreV1().Secrets(ref.Namespace).Get(ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error getting secret %s in namespace %s: %v", ref.Name, ref.Namespace, err)
	}

	credentials := map[string]string{}
	for key, value := range secret.Data {
		credentials[key] = string(value)
	}
	return credentials, nil
}
