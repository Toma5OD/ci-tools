package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/test-infra/prow/flagutil"
	configflagutil "k8s.io/test-infra/prow/flagutil/config"
	prowpluginconfig "k8s.io/test-infra/prow/flagutil/plugins"
	"k8s.io/test-infra/prow/logrusutil"
	"k8s.io/test-infra/prow/plugins"
	"k8s.io/test-infra/prow/pod-utils/downwardapi"

	"github.com/openshift/ci-tools/pkg/config"
)

const cherrypickPlugin = "cherrypick"
const cherrypickRobot = "openshift-cherrypick-robot"

type options struct {
	config          configflagutil.ConfigOptions
	bots            flagutil.Strings
	ignore          flagutil.Strings
	repos           flagutil.Strings
	releaseRepoPath string
	flagutil.GitHubOptions
	pluginConfig prowpluginconfig.PluginOptions
}

func gatherOptions() options {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	fs.Var(&o.bots, "bot", "Check if this bot is a collaborator. Can be passed multiple times.")
	fs.Var(&o.ignore, "ignore", "Ignore a repo or entire org. Formatted org or org/repo. Can be passed multiple times.")
	fs.Var(&o.repos, "repo", "Specifically check only an org/repo. Can be passed multiple times.")
	fs.StringVar(&o.releaseRepoPath, "candidate-path", "", "Path to a openshift/release working copy with a revision to be tested")
	o.pluginConfig.AddFlags(fs)

	o.GitHubOptions.AddFlags(fs)
	o.config.AddFlags(fs)

	if err := fs.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Fatal("could not parse input")
	}
	return o
}

func (o *options) validate() error {
	if len(o.bots.Strings()) < 1 {
		return errors.New("at least one bot must be configured")
	}
	repos := o.repos.Strings()
	if len(repos) == 0 {
		// If we need to find repos, we either need the release repo path, or a proper prow config
		if o.releaseRepoPath == "" {
			if err := o.config.Validate(true); err != nil {
				return fmt.Errorf("candidate-path not provided, and error when validating prow config: %w", err)
			}
		} else {
			if o.config.ConfigPath != "" {
				return errors.New("candidate-path and prow config provided, these are mutually exclusive")
			}
		}
	} else {
		if o.releaseRepoPath != "" || o.config.ConfigPath != "" {
			return errors.New("repo and candidate-path or prow config provided, these are mutually exclusive")
		}
	}

	return o.GitHubOptions.Validate(true)
}

type automationClient interface {
	IsMember(org, user string) (bool, error)
	IsCollaborator(org, repo, user string) (bool, error)
	IsAppInstalled(org, repo string) (bool, error)
	HasPermission(org, repo, user string, permissions ...string) (bool, error)
}

func main() {
	logrusutil.ComponentInit()
	logger := logrus.WithField("component", "check-gh-automation")

	o := gatherOptions()
	if err := o.validate(); err != nil {
		logger.Fatalf("validation error: %v", err)
	}

	var pluginAgent *plugins.ConfigAgent
	if o.pluginConfig.PluginConfigPath != "" {
		logger.Infof("Loading plugin configuration from: %s", o.pluginConfig.PluginConfigPath)
		var err error
		pluginAgent, err = o.pluginConfig.PluginAgent()
		if err != nil {
			logger.Fatalf("Error creating plugin agent: %v", err)
		}
		logger.Info("Plugin configuration loaded successfully.")
	} else {
		logger.Info("No plugin configuration provided, continuing without a plugin agent.")
	}

	client, err := o.GitHubOptions.GitHubClient(false)
	if err != nil {
		logger.Fatalf("error creating client: %v", err)
	}

	repos := determineRepos(o, logger)
	failing, err := checkRepos(repos, o.bots.Strings(), o.ignore.StringSet(), client, logger, pluginAgent)
	if err != nil {
		logger.Fatalf("error checking repos: %v", err)
	}

	if len(failing) > 0 {
		logger.Fatalf("Repo(s) missing github automation: %s", strings.Join(failing, ", "))
	}

	logger.Infof("All repos have github automation configured.")
}

func determineRepos(o options, logger *logrus.Entry) []string {
	repos := o.repos.Strings()
	if len(repos) > 0 {
		return repos
	}

	if o.config.ConfigPath != "" {
		configAgent, err := o.config.ConfigAgent()
		if err != nil {
			logger.Fatalf("error loading prow config: %v", err)
		}
		return sets.List(configAgent.Config().AllRepos)
	}

	return gatherModifiedRepos(o.releaseRepoPath, logger)
}

func checkRepos(repos []string, bots []string, ignore sets.Set[string], client automationClient, logger *logrus.Entry, configAgent *plugins.ConfigAgent) ([]string, error) {
	logger.Infof("checking %d repo(s): %s", len(repos), strings.Join(repos, ", "))
	failing := sets.New[string]()
	for _, orgRepo := range repos {
		split := strings.Split(orgRepo, "/")
		org, repo := split[0], split[1]
		repoLogger := logger.WithFields(logrus.Fields{
			"org":  org,
			"repo": repo,
		})

		if ignore.Has(org) || ignore.Has(orgRepo) {
			repoLogger.Infof("skipping ignored repo")
			continue
		}

		var missingBots []string
		for _, bot := range bots {
			isMember, err := client.IsMember(org, bot)
			if err != nil {
				return nil, fmt.Errorf("unable to determine if: %s is a member of %s: %w", bot, org, err)
			}
			if isMember {
				repoLogger.WithField("bot", bot).Info("bot is an org member")
				continue
			}

			isCollaborator, err := client.IsCollaborator(org, repo, bot)
			if err != nil {
				return nil, fmt.Errorf("unable to determine if: %s is a collaborator on %s/%s: %w", bot, org, repo, err)
			}
			if !isCollaborator {
				missingBots = append(missingBots, bot)
			}
		}

		if len(missingBots) > 0 {
			failing.Insert(orgRepo)
			repoLogger.Errorf("bots that are not collaborators: %s", strings.Join(missingBots, ", "))
		} else {
			repoLogger.Info("all bots are org members or repo collaborators")
		}

		appInstalled, err := client.IsAppInstalled(org, repo)
		if err != nil {
			return nil, fmt.Errorf("unable to determine if openshift-ci app is installed on %s/%s: %w", org, repo, err)
		}

		if configAgent != nil {
			externalPlugins := configAgent.Config().ExternalPlugins[orgRepo]
			if externalPlugins == nil {
				externalPlugins = configAgent.Config().ExternalPlugins[org]
			}
			for _, plugin := range externalPlugins {
				if plugin.Name == cherrypickPlugin {
					isMember, err := client.IsMember(org, cherrypickRobot)
					if err != nil {
						return nil, fmt.Errorf("failed to determine membership status of 'openshift-cherrypick-robot' in '%s': %w", org, err)
					}
					hasAccess, err := client.HasPermission(org, repo, cherrypickRobot, "read", "write", "admin")
					if err != nil {
						return nil, fmt.Errorf("error checking access level (read/write/admin) for 'openshift-cherrypick-robot' in '%s/%s': %w", org, repo, err)
					}
					if !isMember && !hasAccess {
						repoLogger.Infof("'openshift-cherrypick-robot' lacks required permissions (read/write/admin) in '%s/%s'", org, repo)
						failing.Insert(orgRepo)
					} else {
						repoLogger.Infof("'openshift-cherrypick-robot' has sufficient permissions (member or read/write/admin) in '%s/%s'", org, repo)
					}
				}
			}
		}

		if !appInstalled {
			failing.Insert(orgRepo)
			repoLogger.Error("openshift-ci app is not installed for repo")
		} else {
			repoLogger.Info("openshift-ci app is installed for repo")
		}

	}

	return sets.List(failing), nil
}

const maxRepos = 10

func gatherModifiedRepos(releaseRepoPath string, logger *logrus.Entry) []string {
	jobSpec, err := downwardapi.ResolveSpecFromEnv()
	if err != nil {
		logger.Fatalf("error resolving JobSpec: %v", err)
	}
	configs, err := config.GetAddedConfigs(releaseRepoPath, jobSpec.Refs.BaseSHA)
	if err != nil {
		logger.Fatalf("error determining changed configs: %v", err)
	}

	orgRepos := sets.Set[string]{}
	for _, c := range configs {
		path := strings.TrimPrefix(c, config.CiopConfigInRepoPath+"/")
		split := strings.Split(path, "/")
		if split[1] == ".config.prowgen" {
			continue
		}

		orgRepos.Insert(fmt.Sprintf("%s/%s", split[0], split[1]))
	}

	if orgRepos.Len() > maxRepos {
		logger.Warnf("Found %d repos, which is more than we will check for a PR. It is likely that this PR is a config update on many repos, and doesn't need to be checked.", orgRepos.Len())
		return []string{}
	}

	return sets.List(orgRepos)
}
