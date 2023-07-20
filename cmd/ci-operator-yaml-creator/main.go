package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/go-cmp/cmp"
	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/sets"
	git "k8s.io/test-infra/prow/git/v2"
	"sigs.k8s.io/yaml"

	cioperatorapi "github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/api/ocpbuilddata"
	"github.com/openshift/ci-tools/pkg/config"
	"github.com/openshift/ci-tools/pkg/github"
	"github.com/openshift/ci-tools/pkg/github/prcreation"
)

type opts struct {
	prcreation.PRCreationOptions
	ocpBuildDataDir     string
	ciOperatorConfigDir string
	pushCeiling         int
	createPRs           bool
}

func getOpts() (*opts, error) {
	o := opts{}
	o.PRCreationOptions.AddFlags(flag.CommandLine)
	flag.StringVar(&o.ciOperatorConfigDir, "ci-operator-config-dir", "", "Basepath of the ci-operator config")
	flag.StringVar(&o.ocpBuildDataDir, "ocp-build-data-dir", "../ocp-build-data", "Basepath of the ocp build data config")
	_ = flag.Int64("max-concurrency", 4, "Legacy flag that does nothing, the tool can not run concurrently")
	flag.IntVar(&o.pushCeiling, "push-ceiling", 1, "Max number of repos to push an updated .ci-operator.yaml to. Set to 0 for unlimited.")
	flag.BoolVar(&o.createPRs, "create-prs", false, "If the tool should create PRs after pushing")
	flag.Parse()

	if err := o.GitHubOptions.Validate(false); err != nil {
		return nil, fmt.Errorf("faield to validate GitHub options: %w", err)
	}
	if o.ciOperatorConfigDir == "" {
		return nil, errors.New("--ci-operator-config-dir is mandatory")
	}
	if o.ocpBuildDataDir == "" {
		return nil, errors.New("--ocp-build-data-dir is mandatory")
	}

	return &o, nil
}

func main() {
	o, err := getOpts()
	if err != nil {
		logrus.WithError(err).Fatal("failed to get options")
	}

	filter, err := hasOCPBuildDataEntryFilter(o.ocpBuildDataDir)
	if err != nil {
		logrus.WithError(err).Fatal("failed to read ocp build data")
	}

	if err := o.PRCreationOptions.Finalize(); err != nil {
		logrus.WithError(err).Fatal("failed to set up pr creation options")
	}
	gc, err := o.GitHubOptions.GitClient(false)
	if err != nil {
		logrus.WithError(err).Fatal("failed to construct git client")
	}
	defer func() {
		if err := gc.Clean(); err != nil {
			logrus.WithError(err).Error("git client failed to clean")
		}
	}()

	var prCreationOps []prcreation.PrOption
	if !o.createPRs {
		prCreationOps = append(prCreationOps, prcreation.SkipPRCreation())
	}
	prCreationOps = append(prCreationOps, prcreation.PrBody(`
This is an autogenerated PR that updates the `+"`.ci-operator.yaml`"+`
to reference the `+"`build_root_image`"+` found in the [ci-operator-config](https://github.com/openshift/release/tree/master/ci-operator/config)
in the [openshift/release](https://github.com/openshift/release) repository.

This is done in preparation for enabling reading the `+"`build_root`"+` from
your repository rather than the central config in [openshift/release](https://github.com/openshift/release).
This allows to update the `+"`build_root`"+` in lockstep with code changes. For details, please
refer to the [docs](https://docs.ci.openshift.org/docs/architecture/ci-operator/#build-root-image).

Note that enabling this feature is mandatory for all OCP components that have an ART build config.

A second autogenerated PR to the [openshift/release repository](https://github.com/openshift/release)
will enable reading the `+"`build_root`"+` from your repository once this PR was merged.

If you have any questions, please feel free to reach out in the #forum-ocp-testplatform
channel in the CoreOS Slack.`))
	process := process(
		filter,
		github.FileGetterFactory,
		ioutil.WriteFile,
		git.ClientFactoryFrom(gc).ClientFor,
		o.pushCeiling,
		func(localSourceDir, org, repo, targetBranch string) error {
			return o.PRCreationOptions.UpsertPR(localSourceDir, org, repo, targetBranch, "Updating .ci-operator.yaml `build_root_image` from openshift/release", prCreationOps...)
		},
	)

	var errs []error

	abs, err := filepath.Abs(o.ciOperatorConfigDir)
	if err != nil {
		logrus.WithError(err).Fatalf("failed to determine absolute filepath of %s", o.ciOperatorConfigDir)
	}
	err = config.OperateOnCIOperatorConfigDir(abs, func(cfg *cioperatorapi.ReleaseBuildConfiguration, metadata *config.Info) error {
		if err := process(cfg, metadata); err != nil {
			errs = append(errs, err)
		}

		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}

	for _, err := range errs {
		logrus.WithError(err).Error("Encountered error")
	}
	if len(errs) > 0 {
		logrus.Fatal("Encountered errors")
	}
}

func process(
	filter func(*config.Info) bool,
	repoFileGetter func(org, repo, branch string, _ ...github.Opt) github.FileGetter,
	writeFile func(filename string, data []byte, perm fs.FileMode) error,
	clone func(org, repo string) (git.RepoClient, error),
	pushCeiling int,
	createPr func(localSourceDir, org, repo, targetBranch string) error,
) func(cfg *cioperatorapi.ReleaseBuildConfiguration, metadata *config.Info) error {

	var clonesDone int
	var mutex sync.Mutex

	return func(cfg *cioperatorapi.ReleaseBuildConfiguration, metadata *config.Info) error {
		if !filter(metadata) {
			return nil
		}
		if cfg.BuildRootImage == nil || cfg.BuildRootImage.FromRepository || metadata.Variant != "" || (metadata.Metadata.Branch != "master" && metadata.Metadata.Branch != "main") {
			return nil
		}

		if cfg.BuildRootImage.ImageStreamTagReference == nil {
			// TODO: What to do about these?
			return nil
		}

		data, err := repoFileGetter(metadata.Org, metadata.Repo, metadata.Branch)(cioperatorapi.CIOperatorInrepoConfigFileName)
		if err != nil {
			return fmt.Errorf("failed to get %s/%s#%s:%s: %w", metadata.Org, metadata.Repo, metadata.Branch, cioperatorapi.CIOperatorInrepoConfigFileName, err)
		}

		var inrepoconfig cioperatorapi.CIOperatorInrepoConfig
		if err := yaml.Unmarshal(data, &inrepoconfig); err != nil {
			return fmt.Errorf("failed to unmarshal %s/%s#%s:%s: %w", metadata.Org, metadata.Repo, metadata.Branch, cioperatorapi.CIOperatorInrepoConfigFileName, err)
		}

		expected := cioperatorapi.CIOperatorInrepoConfig{
			BuildRootImage: *cfg.BuildRootImage.ImageStreamTagReference,
		}

		l := logrus.WithFields(logrus.Fields{"org": metadata.Org, "repo": metadata.Repo, "branch": metadata.Branch})
		if diff := cmp.Diff(inrepoconfig, expected); diff == "" {
			cfg.BuildRootImage.ImageStreamTagReference = nil
			cfg.BuildRootImage.FromRepository = true
			serialized, err := yaml.Marshal(cfg)
			if err != nil {
				return fmt.Errorf("failed to marshal config after enabling build_root.from_repository: %w", err)
			}
			if err := writeFile(metadata.Filename, serialized, 0644); err != nil {
				return fmt.Errorf("failed to write %s after setting build_root.from_repository: true: %w", metadata.Filename, err)
			}
			l.WithField("file", metadata.Filename).Info("Enabled buiild_root.from_repository")
			return nil
		}
		l.Info(".ci-operator.yaml needs updating")

		expectedSerialized, err := yaml.Marshal(expected)
		if err != nil {
			return fmt.Errorf("failed to marshal %s for %s/%s: %w", cioperatorapi.CIOperatorInrepoConfigFileName, metadata.Org, metadata.Repo, err)
		}

		mutex.Lock()
		if pushCeiling > 0 && clonesDone >= pushCeiling {
			l.Info("Reached push ceiling, not cloning repo")
			mutex.Unlock()
			return nil
		}
		clonesDone++
		mutex.Unlock()

		repo, err := clone(metadata.Org, metadata.Repo)
		if err != nil {
			return fmt.Errorf("failed to clone %s/%s: %w", metadata.Org, metadata.Repo, err)
		}
		defer func() {
			// Creating a PR changes the working dir, if we don't chdir to a valid dir
			// first, the next clone will fail with a "shell-init: error retrieving current directory: getcwd: cannot access parent directories: No such file or directory"
			if err := os.Chdir("/"); err != nil {
				l.WithError(err).Error("failed to chdir to /")
			}
			if err := repo.Clean(); err != nil {
				l.WithError(err).Error("failed to clean local repo")
			}
		}()
		if err := repo.Checkout(metadata.Branch); err != nil {
			return fmt.Errorf("failed to checkout %s in %s/%s: %w", metadata.Branch, metadata.Org, metadata.Repo, err)
		}

		path := filepath.Join(repo.Directory(), cioperatorapi.CIOperatorInrepoConfigFileName)
		if err := ioutil.WriteFile(path, expectedSerialized, 0644); err != nil {
			return fmt.Errorf("falled to write %s for %s/%s: %w", path, metadata.Org, metadata.Repo, err)
		}
		l.WithField("path", path).Info("Wrote .ci-operator.yaml")

		return createPr(repo.Directory(), metadata.Org, metadata.Repo, metadata.Branch)
	}
}

func hasOCPBuildDataEntryFilter(ocpBuilDataDir string) (func(*config.Info) bool, error) {
	configArray, err := ocpbuilddata.LoadImageConfigs(ocpBuilDataDir, ocpbuilddata.MajorMinor{})
	if err != nil {
		return nil, fmt.Errorf("failed to load ocp build data configs: %w", err)
	}
	orgRepoSet := sets.String{}
	for _, entry := range configArray {
		orgRepoSet.Insert(entry.PublicRepo.String())
	}
	logrus.WithField("art-built-repos", orgRepoSet.List()).Info("Constructed the list of art-built repos")

	return func(i *config.Info) bool {
		return orgRepoSet.Has(i.Org + "/" + i.Repo)
	}, nil
}
