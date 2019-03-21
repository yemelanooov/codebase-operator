package gerrit

import (
	"business-app-handler-controller/models"
	"business-app-handler-controller/pkg/apis/edp/v1alpha1"
	ClientSet "business-app-handler-controller/pkg/openshift"
	"errors"
	"fmt"
	imageV1 "github.com/openshift/api/image/v1"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
	"io"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"log"
	"os"
	"os/exec"
	"strings"
	"text/template"
	"time"
)

type gerritConfigGoTemplating struct {
	Lang              string             `json:"lang"`
	Framework         string             `json:"framework"`
	BuildTool         string             `json:"build_tool"`
	RepositoryUrl     *string            `json:"repository_url"`
	Route             v1alpha1.Route     `json:"route"`
	Database          v1alpha1.Database  `json:"database"`
	AppSettings       models.AppSettings `json:"app_settings"`
	DockerRegistryUrl string             `json:"docker_registry_url"`
	TemplatesDir      string             `json:"templates_dir"`
	CloneSshUrl       string             `json:"clone_ssh_url"`
}

func ConfigInit(clientSet ClientSet.ClientSet, appSettings models.AppSettings,
	spec v1alpha1.BusinessApplicationSpec) (*gerritConfigGoTemplating, error) {
	dtrUrl, err := getOpenshiftDockerRegistryUrl(clientSet)
	if err != nil {
		return nil, err
	}

	templatesDir := fmt.Sprintf("%v/oc-templates", appSettings.WorkDir)
	cloneSshUrl := fmt.Sprintf("ssh://project-creator@gerrit.%v:%v/%v", appSettings.CicdNamespace,
		appSettings.GerritSettings.SshPort, appSettings.Name)

	config := gerritConfigGoTemplating{
		DockerRegistryUrl: *dtrUrl,
		Lang:              spec.Lang,
		Framework:         spec.Framework,
		BuildTool:         spec.BuildTool,
		TemplatesDir:      templatesDir,
		CloneSshUrl:       cloneSshUrl,
		AppSettings:       appSettings,
	}
	if spec.Repository != nil {
		config.RepositoryUrl = &spec.Repository.Url
	}
	if spec.Database != nil {
		config.Database = *spec.Database
	}
	if spec.Route != nil {
		config.Route = *spec.Route
	}

	log.Print("Gerrit config has been initialized")

	return &config, nil
}

func getOpenshiftDockerRegistryUrl(clientSet ClientSet.ClientSet) (*string, error) {
	dtrRegistry, err := clientSet.RouteClient.Routes("default").Get("docker-registry", metav1.GetOptions{})
	if err != nil {
		errorMsg := fmt.Sprintf("Unable to get user settings configmap: %v", err)
		log.Println(errorMsg)
		return nil, errors.New(errorMsg)
	}
	log.Printf("Docker registry URL has been retrieved: %v", dtrRegistry.Spec.Host)
	return &dtrRegistry.Spec.Host, nil
}

func PushConfigs(config gerritConfigGoTemplating, appSettings models.AppSettings, clientSet ClientSet.ClientSet) error {
	appTemplatesDir := fmt.Sprintf("%v/%v/deploy-templates", config.TemplatesDir, appSettings.Name)

	err := createDirectory(config.TemplatesDir)
	if err != nil {
		return err
	}

	err = cloneProjectRepoFromGerrit(config, appSettings)
	if err != nil {
		return err
	}

	err = createDirectory(appTemplatesDir)
	if err != nil {
		return err
	}
	templateBasePath := fmt.Sprintf("/usr/local/bin/templates/applications/%v", strings.ToLower(config.Lang))
	templateName := fmt.Sprintf("%v.tmpl", strings.ToLower(config.Framework))
	templatePath := fmt.Sprintf("%v/%v", templateBasePath, templateName)

	err = copyTemplate(templatePath, templateName, config, appSettings)
	if err != nil {
		return err
	}

	err = copyPipelines(appSettings, config)
	if err != nil {
		return nil
	}

	if strings.ToLower(config.Lang) == "javascript" {
		err = copySonarConfigs(config, appSettings)
		if err != nil {
			return err
		}
	}

	err = commitConfigs(config, appSettings.Name)
	if err != nil {
		return err
	}

	err = pushConfigsToGerrit(config, appSettings.Name, appSettings.GerritKeyPath)
	if err != nil {
		return err
	}

	appImageStream, err := getAppImageStream(config)
	if err != nil {
		return err
	}

	err = createS2IImageStream(clientSet, appSettings, appImageStream)
	if err != nil {
		return err
	}

	return nil
}

func cloneProjectRepoFromGerrit(config gerritConfigGoTemplating, appSettings models.AppSettings) error {
	log.Printf("Cloning repo from gerrit using: %v", config.CloneSshUrl)

	cmd := exec.Command("git", "clone", config.CloneSshUrl, fmt.Sprintf("%v/%v",
		config.TemplatesDir, appSettings.Name))
	out, err := cmd.Output()
	log.Printf("Cloning repo %v to %v: Output: %v", config.CloneSshUrl, config.TemplatesDir, out)

	if err != nil {
		return err
	}
	log.Print("Cloning repo has been finished")

	err = copyMessageHookToRepository(config.TemplatesDir, appSettings.Name)
	if err != nil {
		return err
	}

	return nil
}

func copyMessageHookToRepository(templatesDir string, appName string) error {
	log.Printf("Copying message hook to repository")
	messageHookDestination := fmt.Sprintf("%v/%v/.git/hooks/", templatesDir, appName)
	messageHookPath := "/usr/local/bin/configs/commit-msg"
	from, err := os.Open(messageHookPath)
	if err != nil {
		log.Fatal(err)
	}
	defer from.Close()

	to, err := os.OpenFile(messageHookDestination+"commit-msg", os.O_RDWR|os.O_CREATE, 0755)
	if err != nil {
		log.Fatal(err)
	}
	defer to.Close()

	_, err = io.Copy(to, from)
	if err != nil {
		log.Fatal(err)
	}

	return nil
}

func createDirectory(path string) error {
	log.Printf("Creating directory for oc templates: %v", path)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		err = os.MkdirAll(path, 0755)
		if err != nil {
			return err
		}
	}
	log.Printf("Directory %v has been created", path)
	return nil
}

func copyTemplate(templatePath string, templateName string, config gerritConfigGoTemplating, appSettings models.AppSettings) error {
	templatesDest := fmt.Sprintf("%v/%v/deploy-templates/%v.yaml", config.TemplatesDir, appSettings.Name,
		appSettings.Name)

	f, err := os.Create(templatesDest)
	if err != nil {
		return err
	}

	log.Printf("Start rendering openshift templates: %v", templatePath)
	tmpl, err := template.New(templateName).ParseFiles(templatePath)
	if err != nil {
		log.Printf("Unable to parse application deploy template: %v", err)
		return err
	}

	err = tmpl.Execute(f, config)
	if err != nil {
		log.Printf("Unable to render application deploy template: %v", err)
		return err
	}

	log.Printf("Openshift template for application %v has been rendered", appSettings.Name)
	return nil
}

func copyPipelines(appSettings models.AppSettings, config gerritConfigGoTemplating) error {
	pipelinesPath := "/usr/local/bin/pipelines"
	files, err := ioutil.ReadDir(pipelinesPath)
	if err != nil {
		return err
	}
	pipelinesDest := fmt.Sprintf("%v/%v", config.TemplatesDir, appSettings.Name)
	log.Printf("Start copying pipelines to %v", pipelinesDest)

	for _, f := range files {
		input, err := ioutil.ReadFile(pipelinesPath + "/" + f.Name())
		if err != nil {
			return err
		}

		err = ioutil.WriteFile(pipelinesDest+"/"+f.Name(), input, 0755)
		if err != nil {
			return err
		}
	}
	log.Printf("Jenkins pipelines for application %v has been copied", appSettings.Name)
	return nil
}

func copySonarConfigs(config gerritConfigGoTemplating, appSettings models.AppSettings) error {
	sonarConfigPath := fmt.Sprintf("%v/%v/sonar-project.properties", config.TemplatesDir, appSettings.Name)

	if _, err := os.Stat(sonarConfigPath); err == nil {
		return nil

	} else if os.IsNotExist(err) {
		f, err := os.Create(sonarConfigPath)
		if err != nil {
			return err
		}
		tmpl, err := template.New("sonar-project.properties.tmpl").
			ParseFiles("/usr/local/bin/templates/sonar/sonar-project.properties.tmpl")
		if err != nil {
			return err
		}
		err = tmpl.Execute(f, config)
		if err != nil {
			log.Printf("Unable to render sonar configs fo JS app: %v", err)
			return err
		}
		log.Printf("Sonar configs for application %v has been copied", appSettings.Name)
		defer f.Close()
	}

	return nil
}

func commitConfigs(config gerritConfigGoTemplating, appName string) error {
	commitMessage := fmt.Sprintf("Add template for %v", appName)
	r, err := git.PlainOpen(config.TemplatesDir + "/" + appName)
	if err != nil {
		return err
	}

	w, err := r.Worktree()
	if err != nil {
		return err
	}

	_, err = w.Add(".")
	if err != nil {
		return err
	}

	_, err = w.Commit(commitMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "admin",
			Email: "admin@epam-edp.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		return err
	}
	log.Printf("Commit changes has been completed for application %v", appName)
	return nil
}

func pushConfigsToGerrit(gerritConfig gerritConfigGoTemplating, appName string, keyPath string) error {
	auth, err := Auth(keyPath)
	if err != nil {
		return err
	}

	r, err := git.PlainOpen(gerritConfig.TemplatesDir + "/" + appName)
	if err != nil {
		return err
	}

	err = r.Push(&git.PushOptions{
		RemoteName: "origin",
		RefSpecs: []config.RefSpec{
			"refs/heads/*:refs/heads/*",
			"refs/tags/*:refs/tags/*",
		},
		Auth: auth,
	})
	if err != nil {
		return err
	}
	log.Printf("Configs has been pushed successfully for application %v", appName)

	return nil
}

func createS2IImageStream(clientSet ClientSet.ClientSet, appSettings models.AppSettings, is *imageV1.ImageStream) error {
	_, err := clientSet.ImageClient.ImageStreams(appSettings.CicdNamespace).Get(is.Name, metav1.GetOptions{})
	if err != nil && k8serrors.IsNotFound(err) {
		_, err := clientSet.ImageClient.ImageStreams(appSettings.CicdNamespace).Create(is)
		if err != nil {
			return err
		}
		log.Printf("Image stream in Openshift has been created for application %v", appSettings.Name)
	} else {
		log.Printf("Image stream in Openshift for application %v already exist. Creation skipped", appSettings.Name)
	}
	return nil
}

func newS2IReact(config gerritConfigGoTemplating) *imageV1.ImageStream {
	return &imageV1.ImageStream{
		ObjectMeta: metav1.ObjectMeta{
			Name: "s2i-" + strings.ToLower(config.Lang),
		},
		Spec: imageV1.ImageStreamSpec{
			LookupPolicy: imageV1.ImageLookupPolicy{
				Local: false,
			},
			Tags: []imageV1.TagReference{{
				Name:        "latest",
				Annotations: nil,
				From: &corev1.ObjectReference{
					Kind: "DockerImage",
					Name: "epamedp/s2i-nginx:latest",
				},
				ReferencePolicy: imageV1.TagReferencePolicy{
					Type: "Source",
				},
			}},
		},
	}
}

func newS2IJava(config gerritConfigGoTemplating) *imageV1.ImageStream {
	return &imageV1.ImageStream{
		ObjectMeta: metav1.ObjectMeta{
			Name: "s2i-" + strings.ToLower(config.Lang),
		},
		Spec: imageV1.ImageStreamSpec{
			LookupPolicy: imageV1.ImageLookupPolicy{
				Local: false,
			},
			Tags: []imageV1.TagReference{{
				Name:        "latest",
				Annotations: nil,
				From: &corev1.ObjectReference{
					Kind: "DockerImage",
					Name: "epamedp/s2i-java:latest",
				},
				ReferencePolicy: imageV1.TagReferencePolicy{
					Type: "Source",
				},
			}},
		},
	}
}

func newS2IDotNet(config gerritConfigGoTemplating) *imageV1.ImageStream {
	return &imageV1.ImageStream{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "s2i-" + strings.ToLower(config.Lang),
			Annotations: map[string]string{"openshift.io/display-name": ".NET Core Builder Images"},
		},
		Spec: imageV1.ImageStreamSpec{
			LookupPolicy: imageV1.ImageLookupPolicy{
				Local: false,
			},
			Tags: []imageV1.TagReference{{
				Name: "latest",
				Annotations: map[string]string{
					"description": "Build and run .NET Core 2.0 applications on CentOS 7. For more " +
						"information about using this builder image, including OpenShift considerations, " +
						"see https://github.com/redhat-developer/s2i-dotnetcore/tree/master/2.0/build/README.md.",
					"iconClass":                 "icon-dotnet",
					"openshift.io/display-name": ".NET Core 2.0",
					"sampleContextDir":          "app",
					"sampleRef":                 "dotnetcore-2.0",
					"sampleRepo":                "https://github.com/redhat-developer/s2i-dotnetcore-ex.git",
					"supports":                  "dotnet:2.0,dotnet",
					"tags":                      "builder,.net,dotnet,dotnetcore,rh-dotnet20",
					"version":                   "2.0",
				},
				From: &corev1.ObjectReference{
					Kind: "DockerImage",
					Name: "epamedp/dotnet-20-centos7:latest",
				},
				ImportPolicy: imageV1.TagImportPolicy{},
				ReferencePolicy: imageV1.TagReferencePolicy{
					Type: "Source",
				},
			}},
		},
	}
}

func getAppImageStream(config gerritConfigGoTemplating) (*imageV1.ImageStream, error) {
	switch strings.ToLower(config.Lang) {
	case models.JavaScript:
		return newS2IReact(config), nil
	case models.Java:
		return newS2IJava(config), nil
	case models.DotNet:
		return newS2IDotNet(config), nil
	}
	return nil, nil
}