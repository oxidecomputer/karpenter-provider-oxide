/*
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

package ownership

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ClusterIdentityNamespace provides the default cluster ID.
const ClusterIdentityNamespace = "kube-system"

// ErrNoClusterIdentity reports that this cluster could not be identified.
var ErrNoClusterIdentity = errors.New("unable to establish a cluster identity")

// ClusterUID resolves the identity every ownership marker is written and matched under.
func ClusterUID(ctx context.Context, reader client.Reader, configured string) (types.UID, error) {
	if configured != "" {
		return types.UID(configured), nil
	}

	namespace := &corev1.Namespace{}
	if err := reader.Get(
		ctx,
		client.ObjectKey{Name: ClusterIdentityNamespace},
		namespace,
	); err != nil {
		return "", fmt.Errorf(
			"%w: unable to read the %s namespace, whose UID identifies this cluster, %w",
			ErrNoClusterIdentity, ClusterIdentityNamespace, err,
		)
	}
	if namespace.UID == "" {
		return "", fmt.Errorf(
			"%w: the %s namespace has no UID",
			ErrNoClusterIdentity, ClusterIdentityNamespace,
		)
	}
	return namespace.UID, nil
}
