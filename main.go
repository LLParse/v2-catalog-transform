package main

// Transform Rancher catalog into normalized v2 format.

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"

	log "github.com/Sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

type RancherCatalog struct {
	Endpoint  string
	Branch    string
	CloneDir  string
	Templates []*RancherTemplate
	Log       *log.Entry
}

func NewRancherCatalog(url string) *RancherCatalog {
	p := strings.Split(url, "~")
	endpoint := ""
	branch := "master"
	switch len(p) {
	case 2:
		branch = p[1]
		fallthrough
	case 1:
		endpoint = p[0]
	default:
		log.Infof("Invalid URL: %s", url)
		return nil
	}
	return &RancherCatalog{
		Endpoint:  endpoint,
		Branch:    branch,
		Templates: []*RancherTemplate{},
		Log: log.WithFields(log.Fields{
			"endpoint": endpoint,
			"branch":   branch,
		}),
	}
}

func (c *RancherCatalog) Clone() error {
	parts := strings.Split(c.Endpoint, "/")
	c.CloneDir = fmt.Sprintf("output/%s-%s", parts[len(parts)-1], c.Branch)
	out, err := exec.Command("git", "clone", c.Endpoint, "--quiet",
		"--single-branch", "--branch", c.Branch, c.CloneDir).CombinedOutput()
	if err != nil {
		err = errors.New(fmt.Sprintf("[%s] %s", err, string(out)))
	}
	return err
}

func (c *RancherCatalog) Parse() error {
	for _, templateType := range []string{"infra-templates", "templates", "swarm-templates", "mesos-templates"} {
		templateTypeDir := strings.Join([]string{c.CloneDir, templateType}, "/")
		if d, err := os.Open(templateTypeDir); err == nil {
			files, err2 := d.Readdir(-1)
			if err2 != nil {
				return err2
			}
			for _, file := range files {
				if file.IsDir() {
					templateDir := strings.Join([]string{templateTypeDir, file.Name()}, "/")
					c.Templates = append(c.Templates, NewRancherTemplate(c, templateDir))
				}
			}
		}
	}
	for _, t := range c.Templates {
		if err := t.Parse(); err != nil {
			return err
		}
	}
	return nil
}

func (c *RancherCatalog) Transform(preserve *bool) error {
	for _, t := range c.Templates {
		if err := t.Transform(preserve); err != nil {
			return err
		}
	}
	return nil
}

func (c *RancherCatalog) String() string {
	return fmt.Sprintf("endpoint=%s\tbranch=%s", c.Endpoint, c.Branch)
}

type RancherTemplate struct {
	Dir            string
	ConfigFilepath string
	Config         *RancherTemplateConfig
	IconFilepath   string
	Versions       []*RancherTemplateVersion
	Log            *log.Entry
}

func NewRancherTemplate(c *RancherCatalog, templateDir string) *RancherTemplate {
	return &RancherTemplate{
		Dir:      templateDir,
		Versions: []*RancherTemplateVersion{},
		Log: c.Log.WithFields(log.Fields{
			"dir": templateDir,
		}),
	}
}

func (t *RancherTemplate) Parse() error {
	if d, err := os.Open(t.Dir); err != nil {
		return err
	} else {
		files, err2 := d.Readdir(-1)
		if err2 != nil {
			return err2
		}
		for _, file := range files {
			filepath := strings.Join([]string{t.Dir, file.Name()}, "/")
			switch {
			case file.IsDir():
				t.Versions = append(t.Versions, NewRancherTemplateVersion(t, filepath))
			case file.Name() == "config.yml":
				t.ConfigFilepath = filepath
				var data []byte
				if data, err = ioutil.ReadFile(filepath); err == nil {
					config := RancherTemplateConfig{}
					if err = yaml.Unmarshal(data, &config); err == nil {
						t.Config = &config
					}
				}
			case strings.HasPrefix(file.Name(), "catalogIcon-"):
				t.IconFilepath = filepath
			default:
				// t.Log.Warnf("Unrecognized file: %s", filepath)
			}
		}
	}
	for _, v := range t.Versions {
		if err := v.Parse(); err != nil {
			t.Log.Warn(err)
		}
	}
	return nil
}

func (t *RancherTemplate) Transform(preserve *bool) error {
	// adjust and move the config file
	t.Config.DefaultVersion = t.Config.Version
	t.Config.Version = ""
	t.Config.ProjectURL = t.Config.OldProjectURL
	t.Config.OldProjectURL = ""
	if data, err := yaml.Marshal(t.Config); err != nil {
		return err
	} else {
		p := strings.Split(t.ConfigFilepath, "/")
		p[len(p)-1] = "template.yml"
		newConfigFilepath := strings.Join(p, "/")
		if err = ioutil.WriteFile(newConfigFilepath, data, 0644); err != nil {
			return err
		}
		if !*preserve {
			if err = os.Remove(t.ConfigFilepath); err != nil {
				return err
			}
		}
		t.ConfigFilepath = newConfigFilepath
	}

	// move the icon file
	if t.IconFilepath != "" {
		p := strings.Split(t.IconFilepath, "/")
		q := strings.Split(p[len(p)-1], ".")
		p[len(p)-1] = fmt.Sprintf("icon.%s", q[len(q)-1])
		newIconFilepath := strings.Join(p, "/")
		if err := exec.Command("mv", t.IconFilepath, newIconFilepath).Run(); err != nil {
			return err
		}
		t.IconFilepath = newIconFilepath
	}

	// config.yml -> template.yml
	for _, v := range t.Versions {
		if err := v.Transform(preserve); err != nil {
			return err
		}
	}
	return nil
}

type RancherTemplateVersion struct {
	Dir              string
	DockerComposeV1  *DockerComposeV1
	DockerComposeV2  *DockerComposeV2
	RancherComposeV1 *DockerComposeV1
	RancherComposeV2 *DockerComposeV2
	RancherCompose   *RancherCompose
	Log              *log.Entry
}

func NewRancherTemplateVersion(t *RancherTemplate, versionDir string) *RancherTemplateVersion {
	return &RancherTemplateVersion{
		Dir: versionDir,
		Log: t.Log.WithFields(log.Fields{
			"dir": versionDir,
		}),
	}
}

func (v *RancherTemplateVersion) getRancherComposeFilepath(newFilename bool) string {
	filename := "rancher-compose.yml"
	if newFilename {
		filename = "template-version.yml"
	}
	return strings.Join([]string{v.Dir, filename}, "/")
}

func (v *RancherTemplateVersion) getDockerComposeFilepath(newFilename bool) string {
	filename := "docker-compose.yml"
	// filename = "docker-compose.yml.tpl"
	if newFilename {
		filename = "compose.yml"
		// filename = "compose.yml.tpl"
	}

	filepath := strings.Join([]string{v.Dir, filename}, "/")
	return filepath
}

type VersionDetector struct {
	Version string
}

func (v *RancherTemplateVersion) DetectComposeVersion(data []byte) string {
	version := "1"

	vd := VersionDetector{}
	if err := yaml.Unmarshal(data, &vd); err == nil {
		switch vd.Version {
		case "2":
			version = vd.Version
		}
	}

	return version
}

func (v *RancherTemplateVersion) Parse() error {

	if data, err := ioutil.ReadFile(v.getRancherComposeFilepath(false)); err != nil {
		v.Log.Warn("Error reading rancher-compose.yml")
	} else {
		rc := RancherCompose{}
		if err = yaml.Unmarshal(data, &rc); err == nil {
			v.RancherCompose = &rc
		}

		switch v.DetectComposeVersion(data) {
		case "1":
			dc := DockerComposeV1{}
			if err = yaml.Unmarshal(data, &dc); err == nil {
				v.RancherComposeV1 = &dc
			}
		case "2":
			dc := DockerComposeV2{}
			if err = yaml.Unmarshal(data, &dc); err == nil {
				v.RancherComposeV2 = &dc
			}
		}
	}

	if data, err := ioutil.ReadFile(v.getDockerComposeFilepath(false)); err != nil {
		v.Log.Warn("Error reading docker-compose.yml")
	} else {
		switch v.DetectComposeVersion(data) {
		case "1":
			dc := DockerComposeV1{}
			if err = yaml.Unmarshal(data, &dc); err == nil {
				v.DockerComposeV1 = &dc
			}
		case "2":
			dc := DockerComposeV2{}
			if err = yaml.Unmarshal(data, &dc); err == nil {
				v.DockerComposeV2 = &dc
			}
		}
	}

	return nil
}

type Service map[string]map[string]interface{}

func (v *RancherTemplateVersion) merge(a Service, b Service) Service {
	if a == nil {
		return b
	} else if b == nil {
		return a
	}
	for ak, av := range a {
		if b[ak] == nil {
			b[ak] = av
		} else {
			for avk, avv := range av {
				b[ak][avk] = avv
			}
		}
	}
	return b
}

func (v *RancherTemplateVersion) Transform(preserve *bool) error {
	// rename the root folder to catalog version
	if v.RancherCompose.Catalog != nil && v.RancherCompose.Catalog.Version != "" {
		p := strings.Split(v.Dir, "/")
		p[len(p)-1] = v.RancherCompose.Catalog.Version
		newDir := strings.Join(p, "/")
		if err := exec.Command("mv", v.Dir, newDir).Run(); err != nil {
			return err
		}
		v.Dir = newDir
	}

	// write out new rancher-compose.yml
	if data, err := yaml.Marshal(v.RancherCompose.Catalog); err != nil {
		return err
	} else if err = ioutil.WriteFile(v.getRancherComposeFilepath(true), data, 0644); err != nil {
		return err
	}
	if !*preserve {
		if err := os.Remove(v.getRancherComposeFilepath(false)); err != nil {
			return err
		}
	}

	// merge docker/rancher compose into data
	var data []byte
	var err error
	// docker/rancher compose files may be either v1 or v2
	switch {
	case v.DockerComposeV1 != nil && v.RancherComposeV1 != nil:
		v.DockerComposeV1.Services = v.merge(v.DockerComposeV1.Services, v.RancherComposeV1.Services)
		data, err = yaml.Marshal(v.DockerComposeV1)
	case v.DockerComposeV1 != nil && v.RancherComposeV2 != nil:
		v.DockerComposeV1.Services = v.merge(v.DockerComposeV1.Services, v.RancherComposeV2.Services)
		data, err = yaml.Marshal(v.DockerComposeV1)
	case v.DockerComposeV2 != nil && v.RancherComposeV1 != nil:
		v.DockerComposeV2.Services = v.merge(v.DockerComposeV2.Services, v.RancherComposeV1.Services)
		data, err = yaml.Marshal(v.DockerComposeV2)
	case v.DockerComposeV2 != nil && v.RancherComposeV2 != nil:
		v.DockerComposeV2.Services = v.merge(v.DockerComposeV2.Services, v.RancherComposeV2.Services)
		data, err = yaml.Marshal(v.DockerComposeV2)
	}

	if err != nil {
		return err
	} else if len(data) > 0 {
		err = ioutil.WriteFile(v.getDockerComposeFilepath(true), data, 0644)
		if err != nil {
			return err
		}
		if !*preserve {
			if err := os.Remove(v.getDockerComposeFilepath(false)); err != nil {
				return err
			}
		}
	}
	return nil
}

type RancherCompose struct {
	Catalog *RancherComposeCatalog `yaml:".catalog"`
}

type RancherComposeCatalog struct {
	Name                  string            `yaml:"name,omitempty"`
	Version               string            `yaml:"version,omitempty"`
	Description           string            `yaml:"description,omitempty"`
	UUID                  string            `yaml:"uuid,omitempty"`
	MinimumRancherVersion string            `yaml:"minimum_rancher_version,omitempty"`
	MaximumRancherVersion string            `yaml:"maximum_rancher_version,omitempty"`
	UpgradeFrom           string            `yaml:"upgrade_from,omitempty"`
	Labels                map[string]string `yaml:"labels,omitempty"`
	Questions             []Question        `yaml:"questions,omitempty"`
}

type Question struct {
	Variable     string   `yaml:"variable,omitempty"`
	Label        string   `yaml:"label,omitempty"`
	Description  string   `yaml:"description,omitempty"`
	Type         string   `yaml:"type,omitempty"`
	Required     bool     `yaml:"required,omitempty"`
	Default      string   `yaml:"default,omitempty"`
	Group        string   `yaml:"group,omitempty"`
	MinLength    int      `yaml:"min_length,omitempty"`
	MaxLength    int      `yaml:"max_length,omitempty"`
	Min          int      `yaml:"min,omitempty"`
	Max          int      `yaml:"max,omitempty"`
	Options      []string `yaml:"options,omitempty"`
	ValidChars   string   `yaml:"valid_chars,omitempty"`
	InvalidChars string   `yaml:"invalid_chars,omitempty"`
}

type DockerComposeV1 struct {
	// This field exists so we may parse a rancher-compose.yml file as a
	// docker-compose.yml file without treating '.catalog' as an inline service
	Catalog  *RancherComposeCatalog            `yaml:".catalog,omitempty"`
	Services map[string]map[string]interface{} `yaml:"services,inline"`
}

type DockerComposeV2 struct {
	Version  string                            `yaml:"version"`
	Services map[string]map[string]interface{} `yaml:"services"`
	Volumes  map[string]interface{}            `yaml:"volumes,omitempty"`
}

type RancherTemplateConfig struct {
	Name           string            `yaml:"name"`
	Version        string            `yaml:"version,omitempty"`
	Description    string            `yaml:"description,omitempty"`
	DefaultVersion string            `yaml:"default_version,omitempty"`
	Category       string            `yaml:"category,omitempty"`
	OldProjectURL  string            `yaml:"projectURL,omitempty"`
	ProjectURL     string            `yaml:"project_url,omitempty"`
	Labels         map[string]string `yaml:"labels,omitempty"`
}

func main() {
	preserve := flag.Bool("preserve", false, "Preserve original files for comparison & backwards compatibility")
	flag.Parse()
	if urls := flag.Args(); len(urls) == 0 {
		log.Fatalf(`Must provide at least one URL as argument
Example:
  https://git.rancher.io/rancher-catalog~master
  https://github.com/rancher/rancher-catalog~hosted
  https://github.com/rancher/community-catalog`)
	} else {
		if *preserve {
			log.Infof("Preserve enabled")
		}

		catalogs := make(map[string]map[string]*RancherCatalog)
		for _, url := range urls {
			c := NewRancherCatalog(url)
			if catalogs[c.Endpoint] == nil {
				catalogs[c.Endpoint] = make(map[string]*RancherCatalog)
			}
			catalogs[c.Endpoint][c.Branch] = c
			c.Log.Info("Begin")

			var err error
			if err = c.Clone(); err != nil {
				c.Log.Fatalf("Error cloning catalog: %s", err)
			}
			c.Log.Info("Clone Complete")

			if err = c.Parse(); err != nil {
				c.Log.Fatalf("Error parsing catalog: %s", err)
			}
			c.Log.Info("Parse Complete")

			if err = c.Transform(preserve); err != nil {
				c.Log.Fatalf("Error transforming catalog: %s", err)
			}
			c.Log.Info("Transform Complete")
		}
		log.Info("Exiting")
	}
}
