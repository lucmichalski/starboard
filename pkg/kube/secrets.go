package kube

import (
	"context"
	"fmt"

	"github.com/aquasecurity/starboard/pkg/docker"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NewImagePullSecret constructs a new image pull Secret with the specified
// registry server and basic authentication credentials.
func NewImagePullSecret(meta metav1.ObjectMeta, server, username, password string) (*corev1.Secret, error) {
	dockerConfig, err := docker.Config{
		Auths: map[string]docker.Auth{
			server: {
				Username: username,
				Password: password,
				Auth:     docker.NewBasicAuth(username, password),
			},
		},
	}.Write()
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: meta,
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: dockerConfig,
		},
	}, nil
}

// MapContainerNamesToDockerAuths creates the mapping from a container name to the Docker authentication
// credentials for the specified kube.ContainerImages and image pull Secrets.
func MapContainerNamesToDockerAuths(images ContainerImages, secrets []corev1.Secret) (map[string]docker.Auth, error) {
	auths, err := MapDockerRegistryServersToAuths(secrets)
	if err != nil {
		return nil, err
	}

	mapping := make(map[string]docker.Auth)

	for containerName, imageRef := range images {
		server, err := docker.GetServerFromImageRef(imageRef)
		if err != nil {
			return nil, err
		}
		if auth, ok := auths[server]; ok {
			mapping[containerName] = auth
		}
	}

	return mapping, nil
}

// MapDockerRegistryServersToAuths creates the mapping from a Docker registry server
// to the Docker authentication credentials for the specified slice of image pull Secrets.
func MapDockerRegistryServersToAuths(imagePullSecrets []corev1.Secret) (map[string]docker.Auth, error) {
	auths := make(map[string]docker.Auth)
	for _, secret := range imagePullSecrets {
		dockerConfig := &docker.Config{}
		err := dockerConfig.Read(secret.Data[corev1.DockerConfigJsonKey])
		if err != nil {
			return nil, err
		}
		for server, auth := range dockerConfig.Auths {
			host, err := docker.GetHostFromServer(server)
			if err != nil {
				return nil, err
			}
			auths[host] = auth
		}
	}
	return auths, nil
}

func AggregateImagePullSecretsData(images ContainerImages, credentials map[string]docker.Auth) map[string][]byte {
	secretData := make(map[string][]byte)

	for containerName := range images {
		if dockerAuth, ok := credentials[containerName]; ok {
			secretData[fmt.Sprintf("%s.username", containerName)] = []byte(dockerAuth.Username)
			secretData[fmt.Sprintf("%s.password", containerName)] = []byte(dockerAuth.Password)
		}
	}

	return secretData
}

const (
	serviceAccountDefault = "default"
)

// SecretsReader defines methods for reading Secrets.
type SecretsReader interface {
	ListByLocalObjectReferences(ctx context.Context, refs []corev1.LocalObjectReference, ns string) ([]corev1.Secret, error)
	ListByServiceAccount(ctx context.Context, name string, ns string) ([]corev1.Secret, error)
	ListImagePullSecretsByPodSpec(ctx context.Context, spec corev1.PodSpec, ns string) ([]corev1.Secret, error)
}

// NewSecretsReader constructs a new SecretsReader which is using the client-go
// module for interacting with the Kubernetes API server.
func NewSecretsReader(clientset kubernetes.Interface) SecretsReader {
	return &reader{
		clientset: clientset,
	}
}

type reader struct {
	clientset kubernetes.Interface
}

func (r *reader) ListImagePullSecretsByPodSpec(ctx context.Context, spec corev1.PodSpec, ns string) ([]corev1.Secret, error) {
	secrets, err := r.ListByLocalObjectReferences(ctx, spec.ImagePullSecrets, ns)
	if err != nil {
		return nil, err
	}

	serviceAccountName := spec.ServiceAccountName
	if serviceAccountName == "" {
		serviceAccountName = serviceAccountDefault
	}

	serviceAccountSecrets, err := r.ListByServiceAccount(ctx, serviceAccountName, ns)
	if err != nil {
		return nil, err
	}

	return append(secrets, serviceAccountSecrets...), nil
}

func (r *reader) ListByServiceAccount(ctx context.Context, name string, ns string) ([]corev1.Secret, error) {
	sa, err := r.clientset.CoreV1().ServiceAccounts(ns).
		Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting service account by name: %s/%s: %w", ns, name, err)
	}

	return r.ListByLocalObjectReferences(ctx, sa.ImagePullSecrets, ns)
}

func (r *reader) ListByLocalObjectReferences(ctx context.Context, refs []corev1.LocalObjectReference, ns string) ([]corev1.Secret, error) {
	secrets := make([]corev1.Secret, 0)

	for _, secretRef := range refs {
		secret, err := r.clientset.CoreV1().Secrets(ns).
			Get(ctx, secretRef.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting secret by name: %s/%s: %w", ns, secretRef.Name, err)
		}
		secrets = append(secrets, *secret)
	}

	return secrets, nil
}

// NewControllerRuntimeSecretsReader constructs a new SecretsReader which is
// using the client package provided by the controller-runtime libraries for
// interacting with the Kubernetes API server.
func NewControllerRuntimeSecretsReader(client client.Client) SecretsReader {
	return &crReader{client: client}
}

type crReader struct {
	client client.Client
}

func (r *crReader) ListByLocalObjectReferences(ctx context.Context, refs []corev1.LocalObjectReference, ns string) ([]corev1.Secret, error) {
	secrets := make([]corev1.Secret, 0)

	for _, secretRef := range refs {
		var secret corev1.Secret
		err := r.client.Get(ctx, client.ObjectKey{Name: secretRef.Name, Namespace: ns}, &secret)
		if err != nil {
			return nil, fmt.Errorf("getting secret by name: %s/%s: %w", ns, secretRef.Name, err)
		}
		secrets = append(secrets, secret)
	}

	return secrets, nil
}

func (r *crReader) ListByServiceAccount(ctx context.Context, name string, ns string) ([]corev1.Secret, error) {
	var sa corev1.ServiceAccount

	err := r.client.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, &sa)
	if err != nil {
		return nil, fmt.Errorf("getting service account by name: %s/%s: %w", ns, name, err)
	}

	return r.ListByLocalObjectReferences(ctx, sa.ImagePullSecrets, ns)
}

func (r *crReader) ListImagePullSecretsByPodSpec(ctx context.Context, spec corev1.PodSpec, ns string) ([]corev1.Secret, error) {
	secrets, err := r.ListByLocalObjectReferences(ctx, spec.ImagePullSecrets, ns)
	if err != nil {
		return nil, err
	}

	serviceAccountName := spec.ServiceAccountName
	if serviceAccountName == "" {
		serviceAccountName = serviceAccountDefault
	}

	serviceAccountSecrets, err := r.ListByServiceAccount(ctx, serviceAccountName, ns)
	if err != nil {
		return nil, err
	}

	return append(secrets, serviceAccountSecrets...), nil
}
