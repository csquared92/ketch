// Package deploy is purposed to deploy an app.  This concern encompasses creating the app CRD if it doesn't exist,
// possibly creating the app image from source code, and then creating a deployment that will the image in a k8s cluster.
package deploy

import (
	"context"
	"fmt"
	"path"
	"log"
	"time"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ketchv1 "github.com/shipa-corp/ketch/internal/api/v1beta1"
	"github.com/shipa-corp/ketch/internal/build"
	"github.com/shipa-corp/ketch/internal/chart"
	"github.com/shipa-corp/ketch/internal/errors"
)

const (
	defaultTrafficWeight = 100
	minimumSteps         = 2
	maximumSteps         = 100
	defaultProcFile      = "Procfile"
)

// Client represents go sdk k8s client operations that we need.
type Client interface {
	Get(ctx context.Context, key client.ObjectKey, obj runtime.Object) error
	Create(ctx context.Context, obj runtime.Object, opts ...client.CreateOption) error
	Update(ctx context.Context, obj runtime.Object, opts ...client.UpdateOption) error
}

type SourceBuilderFn func(context.Context, *build.CreateImageFromSourceRequest, ...build.Option) error

// Runner is concerned with managing and running the deployment.
type Runner struct {
	params *ChangeSet
}

// New creates a Runner which will execute the deployment.
func New(params *ChangeSet) *Runner {
	var r Runner
	r.params = params
	return &r
}

// Run executes the deployment. This includes creating the application CRD if it doesn't already exist, possibly building
// source code and creating an image and creating and applying a deployment CRD to the cluster.
func (r Runner) Run(ctx context.Context, svc *Services) error {
	app, err := getUpdatedApp(ctx, svc.Client, r.params)
	if err != nil {
		return err
	}

	if r.params.sourcePath != nil {
		return deployFromSource(ctx, svc, app, r.params)
	}

	return deployFromImage(ctx, svc, app, r.params)
}

type appUpdater func(ctx context.Context, app *ketchv1.App, changed bool) error

func getAppWithUpdater(ctx context.Context, client Client, cs *ChangeSet) (*ketchv1.App, appUpdater, error) {
	var app ketchv1.App
	err := client.Get(ctx, types.NamespacedName{Name: cs.appName}, &app)
	if apierrors.IsNotFound(err) {
		if err = validateCreateApp(ctx, client, cs.appName, cs); err != nil {
			return nil, nil, err
		}

		return &app, func(ctx context.Context, app *ketchv1.App, _ bool) error {
			app.ObjectMeta.Name = cs.appName
			app.Spec.Deployments = []ketchv1.AppDeploymentSpec{}
			app.Spec.Ingress = ketchv1.IngressSpec{
				GenerateDefaultCname: true,
			}
			return client.Create(ctx, app)
		}, nil
	}
	if err != nil {
		return nil, nil, err
	}

	return &app, func(ctx context.Context, app *ketchv1.App, changed bool) error {
		if !changed {
			return nil
		}
		return client.Update(ctx, app)
	}, nil

}

func getUpdatedApp(ctx context.Context, client Client, cs *ChangeSet) (*ketchv1.App, error) {
	var app *ketchv1.App
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var changed bool
		a, updater, err := getAppWithUpdater(ctx, client, cs)
		if err != nil {
			return err
		}
		app = a

		if cs.sourcePath != nil {
			if err := validateSourceDeploy(cs); err != nil {
				return err
			}
			builder := cs.getBuilder(app.Spec)
			if builder != app.Spec.Builder {
				app.Spec.Builder = builder
				changed = true
			}
			buildPacks, err := cs.getBuildPacks()
			if err := assign(err, func() error {
				app.Spec.BuildPacks = buildPacks
				changed = true
				return nil
			}); err != nil {
				return err
			}
		}
		if err := validateDeploy(cs, app); err != nil {
			return err
		}

		framework, err := cs.getFramework(ctx, client)
		if err := assign(err, func() error {
			if app.Spec.Framework != "" && framework != app.Spec.Framework {
				return fmt.Errorf("can't change framework once app has been created")
			}
			app.Spec.Framework = framework
			changed = true
			return nil
		}); err != nil {
			return err
		}

		desc, err := cs.getDescription()
		if err := assign(err, func() error {
			app.Spec.Description = desc
			changed = true
			return nil
		}); err != nil {
			return err
		}

		envs, err := cs.getEnvironments()
		if err := assign(err, func() error {
			app.Spec.Env = envs
			changed = true
			return nil
		}); err != nil {
			return err
		}

		secret, err := cs.getDockerRegistrySecret()
		if err := assign(err, func() error {
			app.Spec.DockerRegistry.SecretName = secret
			changed = true
			return nil
		}); err != nil {
			return err
		}

		return updater(ctx, app, changed)
	})
	return app, err
}

func deployFromSource(ctx context.Context, svc *Services, app *ketchv1.App, params *ChangeSet) error {
	ketchYaml, err := params.getKetchYaml()
	if err != nil {
		return err
	}

	var framework ketchv1.Framework
	if err := svc.Client.Get(ctx, types.NamespacedName{Name: app.Spec.Framework}, &framework); err != nil {
		return errors.Wrap(err, "failed to get framework %s", app.Spec.Framework)
	}

	image, _ := params.getImage()
	sourcePath, _ := params.getSourceDirectory()
	sourceProcFilePath := path.Join(sourcePath, defaultProcFile)
	units := params.getUnits()

	if err := svc.Builder(
		ctx,
		&build.CreateImageFromSourceRequest{
			Image:      image,
			AppName:    params.appName,
			Builder:    app.Spec.Builder,
			BuildPacks: app.Spec.BuildPacks,
		},
		build.WithWorkingDirectory(sourcePath),
	); err != nil {
		return err
	}

	imageRequest := ImageConfigRequest{
		imageName:       image,
		secretName:      app.Spec.DockerRegistry.SecretName,
		secretNamespace: framework.Spec.NamespaceName,
		client:          svc.KubeClient,
	}
	imgConfig, err := svc.GetImageConfig(ctx, imageRequest)
	if err != nil {
		return err
	}

	procfile, err := makeProcfile(nil, sourceProcFilePath)
	if err != nil {
		return err
	}

	var updateRequest updateAppCRDRequest

	updateRequest.image = image
	steps, _ := params.getSteps()
	updateRequest.steps = steps
	stepWeight, _ := params.getStepWeight()
	updateRequest.stepWeight = stepWeight
	updateRequest.ketchYaml = ketchYaml
	updateRequest.configFile = imgConfig
	updateRequest.procFile = procfile
	interval, _ := params.getStepInterval()
	updateRequest.stepTimeInterval = interval
	updateRequest.nextScheduledTime = time.Now().Add(interval)
	updateRequest.started = time.Now()
	updateRequest.units = units

	if app, err = updateAppCRD(ctx, svc, params.appName, updateRequest); err != nil {
		return errors.Wrap(err, "deploy from source failed")
	}

	wait, _ := params.getWait()
	if wait {
		timeout, _ := params.getTimeout()
		return svc.Wait(ctx, svc, app, timeout)
	}

	return nil
}

func deployFromImage(ctx context.Context, svc *Services, app *ketchv1.App, params *ChangeSet) error {
	ketchYaml, err := params.getKetchYaml()
	if err != nil {
		return err
	}

	var framework ketchv1.Framework
	if err := svc.Client.Get(ctx, types.NamespacedName{Name: app.Spec.Framework}, &framework); err != nil {
		return errors.Wrap(err, "failed to get framework %q", app.Spec.Framework)
	}

	image, _ := params.getImage()
	units := params.getUnits()

	imageRequest := ImageConfigRequest{
		imageName:       image,
		secretName:      app.Spec.DockerRegistry.SecretName,
		secretNamespace: framework.Spec.NamespaceName,
		client:          svc.KubeClient,
	}
	imgConfig, err := svc.GetImageConfig(ctx, imageRequest)
	if err != nil {
		return err
	}

	procfile, err := makeProcfile(imgConfig, "")
	if err != nil {
		return err
	}

	var updateRequest updateAppCRDRequest
	updateRequest.image = image
	steps, _ := params.getSteps()
	updateRequest.steps = steps
	stepWeight, _ := params.getStepWeight()
	updateRequest.stepWeight = stepWeight
	updateRequest.procFile = procfile
	updateRequest.ketchYaml = ketchYaml
	updateRequest.configFile = imgConfig
	interval, _ := params.getStepInterval()
	updateRequest.stepTimeInterval = interval
	updateRequest.nextScheduledTime = time.Now().Add(interval)
	updateRequest.started = time.Now()
	updateRequest.units = units

	if app, err = updateAppCRD(ctx, svc, params.appName, updateRequest); err != nil {
		return errors.Wrap(err, "deploy from image failed")
	}

	wait, _ := params.getWait()
	if wait {
		timeout, _ := params.getTimeout()
		return svc.Wait(ctx, svc, app, timeout)
	}

	return nil
}

func makeProcfile(cfg *registryv1.ConfigFile, procFileName string) (*chart.Procfile, error) {
	if procFileName != "" {
		// validating of path handled by validateSourceDeploy function
		return chart.NewProcfile(procFileName)
	}

	// no procfile (not building from source)
	cmds := append(cfg.Config.Entrypoint, cfg.Config.Cmd...)
	if len(cmds) == 0 {
		return nil, fmt.Errorf("can't use image, no entrypoint or commands")
	}
	return &chart.Procfile{
		Processes: map[string][]string{
			chart.DefaultRoutableProcessName: cmds,
		},
		RoutableProcessName: chart.DefaultRoutableProcessName,
	}, nil
}

type updateAppCRDRequest struct {
	image             string
	steps             int
	stepWeight        uint8
	procFile          *chart.Procfile
	ketchYaml         *ketchv1.KetchYamlData
	configFile        *registryv1.ConfigFile
	nextScheduledTime time.Time
	started           time.Time
	stepTimeInterval  time.Duration
	units             int
}

func updateAppCRD(ctx context.Context, svc *Services, appName string, args updateAppCRDRequest) (*ketchv1.App, error) {
	var updated ketchv1.App
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := svc.Client.Get(ctx, types.NamespacedName{Name: appName}, &updated); err != nil {
			return errors.Wrap(err, "could not get app to deploy %q", appName)
		}

		log.Println("found app deployments:")
		log.Println(updated.Spec.Deployments)

		processes := make([]ketchv1.ProcessSpec, 0, len(args.procFile.Processes))
		for _, processName := range args.procFile.SortedNames() {
			cmd := args.procFile.Processes[processName]
			processes = append(processes, ketchv1.ProcessSpec{
				Name: processName,
				Cmd:  cmd,
			})
		}
		exposedPorts := make([]ketchv1.ExposedPort, 0, len(args.configFile.Config.ExposedPorts))
		for port := range args.configFile.Config.ExposedPorts {
			exposedPort, err := ketchv1.NewExposedPort(port)
			if err != nil {
				// Shouldn't happen
				return err
			}
			exposedPorts = append(exposedPorts, *exposedPort)
		}

		log.Println("default processes")
		log.Println(processes)

		// check to see if any of the processes differ from previous deployment
		// if they differ overwrite the processes in the current deployment
		// not worrying about canary for now
		if updated.Spec.Deployments != nil && args.steps < 1 {
			log.Println("comparison of found deployment to cli")
			for i := range updated.Spec.Deployments {
				// confirm it is not default config?

				// user specified something different, overwrite
				if len(updated.Spec.Deployments[i].Processes) != len(processes) {
					log.Println("processes differ in length")
					updated.Spec.Deployments[i].Processes = processes
				} else {
					log.Println("processes same in length")
					log.Println(updated.Spec.Deployments[i].Processes)
					log.Println(processes)
					log.Println(len(updated.Spec.Deployments[i].Processes))
					for j := range updated.Spec.Deployments[i].Processes {
						// Something doesn't match so overwrite and break
						log.Println(updated.Spec.Deployments[i].Processes[j].Name)
						log.Println(processes[j].Name)
						if updated.Spec.Deployments[i].Processes[j].Name != processes[j].Name {
							log.Println("processes changed; different names")
							updated.Spec.Deployments[i].Processes = processes
							break
						}
						if len(updated.Spec.Deployments[i].Processes[j].Cmd) != len(processes[j].Cmd) {
							log.Println("processes changed; different cmd length")
							updated.Spec.Deployments[i].Processes = processes
							break
						} else {
							for x := range updated.Spec.Deployments[i].Processes[j].Cmd {
								if updated.Spec.Deployments[i].Processes[j].Cmd[x] != processes[j].Cmd[x] {
									log.Println("cmd changed")
									updated.Spec.Deployments[i].Processes = processes
									break
								}
							}
							// break
						}
					}
				}
				updated.Spec.Deployments[i].Image = args.image
				updated.Spec.Deployments[i].KetchYaml = args.ketchYaml
				updated.Spec.Deployments[i].RoutingSettings = ketchv1.RoutingSettings{
					Weight: defaultTrafficWeight,
				}
				updated.Spec.Deployments[i].ExposedPorts = exposedPorts

				deploymentVersion := 0
				processName := "worker"

				if args.units > 0 {
					s := ketchv1.NewSelector(deploymentVersion, processName)
					if err := updated.SetUnits(s, args.units); err != nil {
						log.Println("error is here")
						return err
					}
				}
				return svc.Client.Update(ctx, &updated)
			}
		}

		// default deployment spec for an app
		deploymentSpec := ketchv1.AppDeploymentSpec{
			Image:     args.image,
			Version:   ketchv1.DeploymentVersion(updated.Spec.DeploymentsCount + 1),
			Processes: processes,
			KetchYaml: args.ketchYaml,
			RoutingSettings: ketchv1.RoutingSettings{
				Weight: defaultTrafficWeight,
			},
			ExposedPorts: exposedPorts,
		}

		if args.steps > 1 {
			nextScheduledTime := metav1.NewTime(args.nextScheduledTime)
			started := metav1.NewTime(args.started)
			updated.Spec.Canary = ketchv1.CanarySpec{
				Steps:             args.steps,
				StepWeight:        args.stepWeight,
				StepTimeInteval:   args.stepTimeInterval,
				NextScheduledTime: &nextScheduledTime,
				CurrentStep:       1,
				Active:            true,
				Started:           &started,
			}

			// set initial weight for canary deployment to zero.
			// App controller will update the weight once all pods for canary will be on running state.
			deploymentSpec.RoutingSettings.Weight = 0

			// For a canary deployment, canary should be enabled by adding another deployment to the deployment list.
			updated.Spec.Deployments = append(updated.Spec.Deployments, deploymentSpec)
		} else {
			updated.Spec.Deployments = []ketchv1.AppDeploymentSpec{deploymentSpec}
		}

		log.Println("new app deployments:")
		log.Println(updated.Spec.Deployments)

		updated.Spec.DeploymentsCount += 1

		// temp variable for testing to see if I spawn the right number of pods
		deploymentVersion := 0
		processName := "worker"

		if args.units > 0 {
			s := ketchv1.NewSelector(deploymentVersion, processName)
			if err := updated.SetUnits(s, args.units); err != nil {
				return err
			}
		}

		return svc.Client.Update(ctx, &updated)
	})
	return &updated, err
}
