package chart

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ketchv1 "github.com/shipa-corp/ketch/internal/api/v1beta1"
	"github.com/shipa-corp/ketch/internal/templates"
	"github.com/shipa-corp/ketch/internal/utils/conversions"
)

func TestNewApplicationChart(t *testing.T) {

	const chartDirectory = "./testdata/charts/"

	frameworkWithClusterIssuer := &ketchv1.Framework{
		ObjectMeta: metav1.ObjectMeta{
			Name: "framework",
		},
		Spec: ketchv1.FrameworkSpec{
			NamespaceName: "ketch-gke",
			IngressController: ketchv1.IngressControllerSpec{
				ClassName:       "ingress-class",
				ServiceEndpoint: "10.10.10.10",
				ClusterIssuer:   "letsencrypt-production",
			},
		},
	}
	frameworkWithoutClusterIssuer := &ketchv1.Framework{
		ObjectMeta: metav1.ObjectMeta{
			Name: "framework",
		},
		Spec: ketchv1.FrameworkSpec{
			NamespaceName: "ketch-gke",
			IngressController: ketchv1.IngressControllerSpec{
				ClassName:       "gke",
				ServiceEndpoint: "20.20.20.20",
			},
		},
	}
	exportedPorts := map[ketchv1.DeploymentVersion][]ketchv1.ExposedPort{
		3: {{Port: 9090, Protocol: "TCP"}},
		4: {{Port: 9091, Protocol: "TCP"}},
	}
	memorySize := resource.NewQuantity(5*1024*1024*1024, resource.BinarySI)
	cores := resource.NewMilliQuantity(5300, resource.DecimalSI)
	dashboard := &ketchv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dashboard",
		},
		Spec: ketchv1.AppSpec{
			Deployments: []ketchv1.AppDeploymentSpec{
				{
					Image:   "shipasoftware/go-app:v1",
					Version: 3,
					Processes: []ketchv1.ProcessSpec{
						{
							Name:  "web",
							Units: conversions.IntPtr(3),
							Cmd:   []string{"python"},
							Env: []ketchv1.Env{
								{Name: "TEST_API_KEY", Value: "SECRET"},
								{Name: "TEST_API_URL", Value: "example.com"},
							},
							Resources: &v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceCPU:    *memorySize,
									v1.ResourceMemory: *cores,
								},
								Limits: v1.ResourceList{
									v1.ResourceCPU:    *memorySize,
									v1.ResourceMemory: *cores,
								},
							},
							Volumes: []v1.Volume{{
								Name: "test-volume",
								VolumeSource: v1.VolumeSource{
									AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{
										VolumeID: "volume-id",
										FSType:   "ext4",
									},
								},
							}},
							VolumeMounts: []v1.VolumeMount{{
								MountPath: "/test-ebs",
								Name:      "test-volume",
							}},
						},
						{Name: "worker", Units: conversions.IntPtr(1), Cmd: []string{"celery"}},
					},
					RoutingSettings: ketchv1.RoutingSettings{
						Weight: 30,
					},
					ImagePullSecrets: []v1.LocalObjectReference{
						{Name: "registry-secret"},
						{Name: "private-registry-secret"},
					},
				},
				{
					Image:   "shipasoftware/go-app:v2",
					Version: 4,
					Processes: []ketchv1.ProcessSpec{
						{
							Name:  "web",
							Units: conversions.IntPtr(3),
							Cmd:   []string{"python"},
						},
						{Name: "worker", Units: conversions.IntPtr(1), Cmd: []string{"celery"}},
					},
					RoutingSettings: ketchv1.RoutingSettings{
						Weight: 70,
					},
				},
			},
			Env: []ketchv1.Env{
				{Name: "VAR", Value: "VALUE"},
			},
			Framework: "framework",
			DockerRegistry: ketchv1.DockerRegistrySpec{
				SecretName: "default-image-pull-secret",
			},
			Ingress: ketchv1.IngressSpec{
				GenerateDefaultCname: true,
				Cnames: []ketchv1.Cname{
					{
						Name:   "theketch.io",
						Secure: true,
					}, {
						Name:   "app.theketch.io",
						Secure: true,
					},
				},
			},
			Labels: []ketchv1.MetadataItem{{
				Apply:             map[string]string{"theketch.io/test-label": "test-label-value"},
				DeploymentVersion: 3,
				ProcessName:       "web",
				Target: ketchv1.Target{
					APIVersion: "v1",
					Kind:       "Deployment",
				},
			}, {
				Apply: map[string]string{"theketch.io/test-label-all": "test-label-value-all"},
				Target: ketchv1.Target{
					APIVersion: "v1",
					Kind:       "Deployment",
				},
			}},
			Annotations: []ketchv1.MetadataItem{{
				Apply:             map[string]string{"theketch.io/test-annotation": "test-annotation-value"},
				DeploymentVersion: 4,
				ProcessName:       "web",
				Target: ketchv1.Target{
					APIVersion: "v1",
					Kind:       "Service",
				},
			}},
		},
	}

	// convertSecureEndpoints returns a copy of app with Cnames made not secure
	convertSecureEndpoints := func(app *ketchv1.App) *ketchv1.App {
		out := *app
		out.Spec.Ingress.Cnames = []ketchv1.Cname{}
		for _, cname := range app.Spec.Ingress.Cnames {
			out.Spec.Ingress.Cnames = append(out.Spec.Ingress.Cnames, ketchv1.Cname{Name: cname.Name, Secure: false})
		}
		return &out
	}

	// addAppSecret returns a copy of app with a SecretNames set in the app.Spec
	addAppSecret := func(app *ketchv1.App, secrets []string) *ketchv1.App {
		out := *app
		out.Spec.SecretNames = secrets
		return &out
	}

	tests := []struct {
		name        string
		application *ketchv1.App
		framework   *ketchv1.Framework
		opts        []Option
		group       string

		wantYamlsFilename string
		wantErr           bool
	}{
		{
			name: "istio templates with cluster issuer",
			opts: []Option{
				WithTemplates(templates.IstioDefaultTemplates),
				WithExposedPorts(exportedPorts),
			},
			application:       dashboard,
			framework:         frameworkWithClusterIssuer,
			wantYamlsFilename: "dashboard-istio-cluster-issuer",
		},
		{
			name: "istio templates without cluster issuer",
			opts: []Option{
				WithTemplates(templates.IstioDefaultTemplates),
				WithExposedPorts(exportedPorts),
			},
			application:       convertSecureEndpoints(dashboard),
			framework:         frameworkWithoutClusterIssuer,
			wantYamlsFilename: "dashboard-istio",
		},
		{
			name: "traefik templates with cluster issuer",
			opts: []Option{
				WithTemplates(templates.TraefikDefaultTemplates),
				WithExposedPorts(exportedPorts),
			},
			application:       dashboard,
			framework:         frameworkWithClusterIssuer,
			wantYamlsFilename: "dashboard-traefik-cluster-issuer",
		},
		{
			name: "traefik templates without cluster issuer",
			opts: []Option{
				WithTemplates(templates.TraefikDefaultTemplates),
				WithExposedPorts(exportedPorts),
			},
			application:       convertSecureEndpoints(dashboard),
			framework:         frameworkWithoutClusterIssuer,
			wantYamlsFilename: "dashboard-traefik",
		},
		{
			name: "traefik templates with cluster issuer and resource requirements",
			opts: []Option{
				WithTemplates(templates.TraefikDefaultTemplates),
				WithExposedPorts(exportedPorts),
			},
			application:       dashboard,
			framework:         frameworkWithClusterIssuer,
			wantYamlsFilename: "dashboard-traefik-cluster-issuer",
		},
		{
			name: "traefik templates with cluster issuer w/ alternate group",
			opts: []Option{
				WithTemplates(templates.TraefikDefaultTemplates),
				WithExposedPorts(exportedPorts),
			},
			application:       dashboard,
			framework:         frameworkWithClusterIssuer,
			group:             "shipa.io",
			wantYamlsFilename: "dashboard-traefik-cluster-issuer-shipa",
		},
		{
			name: "traefik templates with app secret",
			opts: []Option{
				WithTemplates(templates.TraefikDefaultTemplates),
				WithExposedPorts(exportedPorts),
			},
			application:       addAppSecret(convertSecureEndpoints(dashboard), []string{"foobar", "poobar"}),
			framework:         frameworkWithoutClusterIssuer,
			group:             "theketch.io",
			wantYamlsFilename: "dashboard-traefik-secret",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.group != "" {
				original := ketchv1.Group
				ketchv1.Group = tt.group
				defer func() {
					ketchv1.Group = original
				}()
			}
			got, err := New(tt.application, tt.framework, tt.opts...)
			if tt.wantErr {
				require.NotNil(t, err, "New() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			require.Nil(t, err)

			expectedFilename := filepath.Join(chartDirectory, fmt.Sprintf("%s.yaml", tt.wantYamlsFilename))
			actualFilename := filepath.Join(chartDirectory, fmt.Sprintf("%s-output.yaml", tt.wantYamlsFilename))

			chartConfig := ChartConfig{
				Version: "0.0.1",
				AppName: tt.application.Name,
			}
			client := HelmClient{cfg: &action.Configuration{KubeClient: &fake.PrintingKubeClient{}, Releases: storage.Init(driver.NewMemory())}, namespace: tt.framework.Spec.NamespaceName}

			release, err := client.UpdateChart(*got, chartConfig, func(install *action.Install) {
				install.DryRun = true
				install.ClientOnly = true
			})
			require.Nil(t, err, "error = %v", err)

			actualManifests := strings.TrimSpace(release.Manifest)
			err = ioutil.WriteFile(actualFilename, []byte(actualManifests), 0755)
			require.Nil(t, err)
			expected, err := ioutil.ReadFile(expectedFilename)
			require.Nil(t, err)
			require.Equal(t, string(expected), actualManifests)
		})
	}
}

func TestNewIngress(t *testing.T) {
	tests := []struct {
		name          string
		cnames        ketchv1.CnameList
		clusterIssuer string
		appSecrets    []string
		expected      *ingress
		expectedError error
	}{
		{
			name: "happy",
			cnames: ketchv1.CnameList{
				{
					Name:   "a.name",
					Secure: true,
				},
				{
					Name: "b.name",
				},
			},
			clusterIssuer: "test-cluster-issuer",
			expected: &ingress{
				Https: []httpsEndpoint{{Cname: "a.name", SecretName: "-cname-2bffdc1c076b2cc72660", ClusterIssuer: "test-cluster-issuer"}},
				Http:  []string{"b.name"},
			},
		},
		{
			name: "happy - no https, no cluster issuer",
			cnames: ketchv1.CnameList{
				{
					Name: "a.name",
				},
				{
					Name: "b.name",
				},
			},
			expected: &ingress{
				Http: []string{"a.name", "b.name"},
			},
		},
		{
			name: "happy - app secret",
			cnames: ketchv1.CnameList{
				{
					Name:   "a.name",
					Secure: true,
				},
				{
					Name: "b.name",
				},
			},
			appSecrets: []string{"app-secret"},
			expected: &ingress{
				Https: []httpsEndpoint{{Cname: "a.name", SecretName: "app-secret", ClusterIssuer: "app-secret-clusterissuer"}},
				Http:  []string{"b.name"},
			},
		},
		{
			name: "sad - no cluster issuer",
			cnames: ketchv1.CnameList{
				{
					Name:   "a.name",
					Secure: true,
				},
			},
			expectedError: errors.New("secure cnames require a framework.Ingress.ClusterIssuer to be specified"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := ketchv1.App{
				Spec: ketchv1.AppSpec{
					Ingress: ketchv1.IngressSpec{
						Cnames: tt.cnames,
					},
					SecretNames: tt.appSecrets,
				},
			}
			framework := ketchv1.Framework{
				Spec: ketchv1.FrameworkSpec{
					IngressController: ketchv1.IngressControllerSpec{
						ClusterIssuer: tt.clusterIssuer,
					},
				},
			}
			issuer, err := newIngress(app, framework)
			if tt.expectedError != nil {
				require.EqualError(t, err, tt.expectedError.Error())
			} else {
				require.Equal(t, tt.expected, issuer)
			}
		})
	}
}
