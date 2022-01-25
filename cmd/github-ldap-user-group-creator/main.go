package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"sync"

	"github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/test-infra/prow/flagutil"
	"k8s.io/test-infra/prow/interrupts"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/logrusutil"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	userv1 "github.com/openshift/api/user/v1"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/group"
)

type options struct {
	kubernetesOptions flagutil.KubernetesOptions

	mappingFile    string
	logLevel       string
	dryRun         bool
	groupsFile     string
	configFile     string
	maxConcurrency int
}

func parseOptions() *options {
	opts := &options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	opts.kubernetesOptions.AddFlags(fs)
	fs.StringVar(&opts.logLevel, "log-level", "info", fmt.Sprintf("Log level is one of %v.", logrus.AllLevels))
	fs.BoolVar(&opts.dryRun, "dry-run", true, "Whether to run the controller-manager with dry-run")
	fs.StringVar(&opts.mappingFile, "mapping-file", "", "File to the mapping results of m(github_login)=kerberos_id.")
	fs.StringVar(&opts.groupsFile, "groups-file", "", "The yaml file storing the groups")
	fs.StringVar(&opts.configFile, "config-file", "", "The yaml file storing the config file for the groups")
	flag.IntVar(&opts.maxConcurrency, "concurrency", 60, "Maximum number of concurrent in-flight goroutines to handle groups.")
	if err := fs.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Fatal("could not parse args")
	}
	return opts
}

func (o *options) validate() error {
	level, err := logrus.ParseLevel(o.logLevel)
	if err != nil {
		return fmt.Errorf("invalid log level specified: %w", err)
	}
	logrus.SetLevel(level)

	if o.mappingFile == "" {
		return fmt.Errorf("--mapping-file must not be empty")
	}
	if o.groupsFile == "" {
		return fmt.Errorf("--groups-file must not be empty")
	}

	return nil
}

const (
	appCIContextName = string(api.ClusterAPPCI)
	toolName         = "github-ldap-user-group-creator"
)

func addSchemes() error {
	if err := userv1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add userv1 to scheme: %w", err)
	}
	return nil
}

func main() {
	logrusutil.ComponentInit()

	opts := parseOptions()

	if err := opts.validate(); err != nil {
		logrus.WithError(err).Fatal("failed to validate the option")
	}

	var config *group.Config
	if opts.configFile != "" {
		loadedConfig, err := group.LoadConfig(opts.configFile)
		if err != nil {
			logrus.WithError(err).Fatal("failed to load config")
		}
		config = loadedConfig
	}

	if err := addSchemes(); err != nil {
		logrus.WithError(err).Fatal("failed to add schemes")
	}

	kubeconfigs, err := opts.kubernetesOptions.LoadClusterConfigs()
	if err != nil {
		logrus.WithError(err).Fatal("failed to load kubeconfigs")
	}

	inClusterConfig, hasInClusterConfig := kubeconfigs[kube.InClusterContext]
	delete(kubeconfigs, kube.InClusterContext)
	delete(kubeconfigs, kube.DefaultClusterAlias)

	if _, hasAppCi := kubeconfigs[appCIContextName]; !hasAppCi {
		if !hasInClusterConfig {
			logrus.WithError(err).Fatalf("had no context for '%s' and loading InClusterConfig failed", appCIContextName)
		}
		logrus.Infof("use InClusterConfig for %s", appCIContextName)
		kubeconfigs[appCIContextName] = inClusterConfig
	}

	kubeConfig := kubeconfigs[appCIContextName]
	appCIClient, err := ctrlruntimeclient.New(&kubeConfig, ctrlruntimeclient.Options{})
	if err != nil {
		logrus.WithError(err).Fatalf("could not create client")
	}

	clients := map[string]ctrlruntimeclient.Client{}
	clusters := sets.NewString()
	for cluster, config := range kubeconfigs {
		clusters.Insert(cluster)
		cluster, config := cluster, config
		if cluster == appCIContextName {
			continue
		}
		client, err := ctrlruntimeclient.New(&config, ctrlruntimeclient.Options{})
		if err != nil {
			logrus.WithError(err).WithField("cluster", cluster).Fatal("could not create client for cluster")
		}
		clients[cluster] = client
	}

	clients[appCIContextName] = appCIClient

	ctx := interrupts.Context()

	mapping, err := func(path string) (map[string]string, error) {
		logrus.WithField("path", path).Debug("Loading the mapping file ...")
		bytes, err := ioutil.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read file %s: %w", path, err)
		}
		var mapping map[string]string
		if err := yaml.Unmarshal(bytes, &mapping); err != nil {
			return nil, fmt.Errorf("failed to unmarshal: %w", err)
		}
		return mapping, nil
	}(opts.mappingFile)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to load the mapping")
	}

	data, err := ioutil.ReadFile(opts.groupsFile)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to read the group file")
	}
	roverGroups := map[string][]string{}
	if err := yaml.Unmarshal(data, &roverGroups); err != nil {
		logrus.WithError(err).Fatal("Failed to unmarshal groups")
	}

	if ciAdmins, ok := roverGroups[api.CIAdminsGroupName]; !ok {
		logrus.WithField("groupName", api.CIAdminsGroupName).Fatal("Failed to find ci-admins group")
	} else if l := len(ciAdmins); l < 3 {
		logrus.WithField("groupName", api.CIAdminsGroupName).WithField("len", l).Fatal("Require at least 3 members of ci-admins group")
	}

	groups, err := makeGroups(mapping, roverGroups, config, clusters)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to make groups")
	}

	if err := ensureGroups(ctx, clients, groups, opts.maxConcurrency, opts.dryRun); err != nil {
		logrus.WithError(err).Fatal("could not ensure groups")
	}
}

type GroupClusters struct {
	Clusters sets.String
	Group    *userv1.Group
}

func makeGroups(mapping map[string]string, roverGroups map[string][]string, config *group.Config, clusters sets.String) (map[string]GroupClusters, error) {
	groups := map[string]GroupClusters{}
	var errs []error
	clustersExceptHive := clusters.Difference(sets.NewString(string(api.HiveCluster)))
	for githubLogin, kerberosId := range mapping {
		groupName := api.GitHubUserGroup(githubLogin)
		groups[groupName] = GroupClusters{
			Clusters: clustersExceptHive,
			Group: &userv1.Group{
				ObjectMeta: metav1.ObjectMeta{Name: groupName, Labels: map[string]string{api.DPTPRequesterLabel: toolName}},
				Users:      sets.NewString(githubLogin, kerberosId).Delete("").List(),
			},
		}
	}

	for k, v := range roverGroups {
		oldGroupName := k
		groupName := k
		clustersForRoverGroup := clusters
		labels := map[string]string{api.DPTPRequesterLabel: toolName}
		if v, ok := config.Groups[k]; ok {
			resolved := v.ResolveClusters(config.ClusterGroups)
			if resolved.Len() > 0 {
				logrus.WithField("groupName", groupName).WithField("clusters", resolved.List()).
					Info("Group does not exists on all clusters")
				clustersForRoverGroup = resolved
			}
			if v.RenameTo != "" {
				logrus.WithField("old", oldGroupName).WithField("new", v.RenameTo).
					Info("Group is renamed")
				groupName = v.RenameTo
				labels["rover-group-name"] = oldGroupName
			}
		}
		if _, ok := groups[groupName]; ok {
			errs = append(errs, fmt.Errorf("group %s has been defined already", groupName))
		}
		groups[groupName] = GroupClusters{
			Clusters: clustersForRoverGroup,
			Group: &userv1.Group{
				ObjectMeta: metav1.ObjectMeta{Name: groupName, Labels: labels},
				Users:      sets.NewString(v...).Delete("").List(),
			},
		}
	}
	return groups, kerrors.NewAggregate(errs)
}

func ensureGroups(ctx context.Context, clients map[string]ctrlruntimeclient.Client, groupsToCreate map[string]GroupClusters, maxConcurrency int, dryRun bool) error {
	var errs []error

	handleGroup := func(cluster string, client ctrlruntimeclient.Client, group *userv1.Group) error {
		if err := validate(group); err != nil {
			return fmt.Errorf("attempt to create invalid group %s on cluster %s: %w", group.Name, cluster, err)
		}
		logger := logrus.WithFields(logrus.Fields{
			"cluster":    cluster,
			"group.Name": group.Name,
		})
		logger.Info("Upserting group ...")
		if dryRun {
			return nil
		}
		if _, err := UpsertGroup(ctx, client, group); err != nil {
			return fmt.Errorf("failed to upsert group %s on cluster %s: %w", group.Name, cluster, err)
		}
		logger.Info("Upserted group")
		return nil
	}

	for cluster, client := range clients {
		listOption := ctrlruntimeclient.MatchingLabels{
			api.DPTPRequesterLabel: toolName,
		}
		groups := &userv1.GroupList{}
		if err := client.List(ctx, groups, listOption); err != nil {
			errs = append(errs, fmt.Errorf("failed to list groups on cluster %s: %w", cluster, err))
		} else {
			for _, group := range groups.Items {
				var shouldDelete bool
				if groupClusters, ok := groupsToCreate[group.Name]; !ok {
					shouldDelete = true
				} else if !groupClusters.Clusters.Has(cluster) {
					shouldDelete = true
				}
				if shouldDelete {
					if group.Name == api.CIAdminsGroupName {
						// should never happen
						errs = append(errs, fmt.Errorf("attempt to delete group %s on cluster %s", group.Name, cluster))
						continue
					}
					logrus.WithField("cluster", cluster).WithField("group.Name", group.Name).Info("Deleting group ...")
					if dryRun {
						continue
					}
					if err := client.Delete(ctx, &userv1.Group{ObjectMeta: metav1.ObjectMeta{Name: group.Name}}); err != nil && !errors.IsNotFound(err) {
						errs = append(errs, fmt.Errorf("failed to delete group %s on cluster %s: %w", group.Name, cluster, err))
						continue
					}
					logrus.WithField("cluster", cluster).WithField("group.Name", group.Name).Info("Deleted group")
				}
			}
		}

		errLock := &sync.Mutex{}
		sem := semaphore.NewWeighted(int64(maxConcurrency))
		for _, groupClusters := range groupsToCreate {
			if !groupClusters.Clusters.Has(cluster) {
				continue
			}
			group := groupClusters.Group.DeepCopy()
			if err := sem.Acquire(ctx, 1); err != nil {
				return fmt.Errorf("failed to acquire semaphore: %w", err)
			}
			go func(cluster string, client ctrlruntimeclient.Client, group *userv1.Group) {
				defer sem.Release(1)
				if err := handleGroup(cluster, client, group); err != nil {
					errLock.Lock()
					errs = append(errs, err)
					errLock.Unlock()
				}
			}(cluster, client, group)
		}
		if err := sem.Acquire(ctx, int64(maxConcurrency)); err != nil {
			logrus.WithError(err).Fatal("failed to acquire semaphore while waiting all workers to finish")
		}
	}
	return kerrors.NewAggregate(errs)
}

func validate(group *userv1.Group) error {
	if group.Name == "" {
		return fmt.Errorf("group name cannot be empty")
	}
	members := sets.NewString()
	for _, m := range group.Users {
		if m == "" {
			return fmt.Errorf("member name in group cannot be empty")
		}
		if members.Has(m) {
			return fmt.Errorf("duplicate member: %s", m)
		}
		members.Insert(m)
	}
	return nil
}

func UpsertGroup(ctx context.Context, client ctrlruntimeclient.Client, group *userv1.Group) (created bool, err error) {
	err = client.Create(ctx, group.DeepCopy())
	if err == nil {
		return true, nil
	}
	if !errors.IsAlreadyExists(err) {
		return false, err
	}
	existing := &userv1.Group{}
	if err := client.Get(ctx, ctrlruntimeclient.ObjectKey{Name: group.Name}, existing); err != nil {
		return false, err
	}
	if equality.Semantic.DeepEqual(group.Users, existing.Users) {
		return false, nil
	}
	if err := client.Delete(ctx, existing); err != nil {
		return false, fmt.Errorf("delete failed: %w", err)
	}
	// Recreate counts as "Update"
	return false, client.Create(ctx, group)
}
