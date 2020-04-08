package diffs

import (
	"fmt"
	"strings"

	"github.com/getlantern/deepcopy"

	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/sets"

	pjapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	prowconfig "k8s.io/test-infra/prow/config"

	cioperatorapi "github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/jobconfig"

	"github.com/openshift/ci-tools/pkg/config"
)

const (
	logRepo       = "repo"
	logJobName    = "job-name"
	logCiopConfig = "ciop-config"

	// ConfigInRepoPath is the prow config path from release repo
	ConfigInRepoPath = "core-services/prow/02_config/_config.yaml"
	// PluginsInRepoPath is the prow plugins config path from release repo
	PluginsInRepoPath = "core-services/prow/02_config/_plugins.yaml"
	// JobConfigInRepoPath is the prowjobs path from release repo
	JobConfigInRepoPath = "ci-operator/jobs"
	// CIOperatorConfigInRepoPath is the ci-operator config path from release repo
	CIOperatorConfigInRepoPath = "ci-operator/config"

	chosenJob            = "Job has been chosen for rehearsal"
	newCiopConfigMsg     = "New ci-operator config file"
	changedCiopConfigMsg = "ci-operator config file changed"
)

// GetChangedCiopConfigs identifies CI Operator configurations that are new or have changed and
// determines for each which jobs are impacted if job-specific changes were made
func GetChangedCiopConfigs(masterConfig, prConfig config.ByFilename, logger *logrus.Entry) (config.ByFilename, map[string]sets.String) {
	ret := config.ByFilename{}
	affectedJobs := map[string]sets.String{}

	for filename, newConfig := range prConfig {
		oldConfig, ok := masterConfig[filename]
		jobs := sets.NewString()

		// new ciop config
		if !ok {
			ret[filename] = newConfig
			logger.WithField(logCiopConfig, filename).Info(newCiopConfigMsg)
			continue
		}

		withoutTests := func(in cioperatorapi.ReleaseBuildConfiguration) cioperatorapi.ReleaseBuildConfiguration {
			var out cioperatorapi.ReleaseBuildConfiguration
			if err := deepcopy.Copy(&out, &in); err != nil {
				logrus.WithError(err).Warn("Could not deep copy configuration.") // this is a programming error
				return out
			}
			out.Tests = nil
			return out
		}

		if !equality.Semantic.DeepEqual(withoutTests(oldConfig.Configuration), withoutTests(newConfig.Configuration)) {
			logger.WithField(logCiopConfig, filename).Info(changedCiopConfigMsg)
			ret[filename] = newConfig
			continue
		}

		oldTests := getTestsByName(oldConfig.Configuration.Tests)
		newTests := getTestsByName(newConfig.Configuration.Tests)

		for as, test := range newTests {
			if !equality.Semantic.DeepEqual(oldTests[as], test) {
				logger.WithField(logCiopConfig, filename).Info(changedCiopConfigMsg)
				ret[filename] = newConfig
				jobs.Insert(as)
			}
		}

		if len(jobs) > 0 {
			affectedJobs[filename] = jobs
		}
	}
	return ret, affectedJobs
}

// GetChangedPresubmits returns a mapping of repo to presubmits to execute.
func GetChangedPresubmits(prowMasterConfig, prowPRConfig *prowconfig.Config, logger *logrus.Entry) config.Presubmits {
	ret := config.Presubmits{}

	masterJobs := getJobsByRepoAndName(prowMasterConfig.JobConfig.PresubmitsStatic)
	for repo, jobs := range prowPRConfig.JobConfig.PresubmitsStatic {
		for _, job := range jobs {
			masterJob := masterJobs[repo][job.Name]
			logFields := logrus.Fields{logRepo: repo, logJobName: job.Name}

			if job.Agent == string(pjapi.KubernetesAgent) {
				// If the agent was changed and is a kubernetes agent, just choose the job for rehearse.
				if masterJob.Agent != job.Agent {
					logger.WithFields(logFields).Info(chosenJob)
					ret.Add(repo, job)
				} else if !equality.Semantic.DeepEqual(masterJob.Spec, job.Spec) {
					logger.WithFields(logFields).Info(chosenJob)
					ret.Add(repo, job)
				} else if masterJob.Optional && !job.Optional {
					logger.WithFields(logFields).Info(chosenJob)
					ret.Add(repo, job)
				} else if !masterJob.AlwaysRun && job.AlwaysRun {
					logger.WithFields(logFields).Info(chosenJob)
					ret.Add(repo, job)
				} else if masterJob.Cluster != job.Cluster {
					logger.WithFields(logFields).Info(chosenJob)
					ret.Add(repo, job)
				}
			}
		}
	}
	return ret
}

// To compare two maps of slices, instead of iterating through the slice
// and compare the same key and index of the other map of slices,
// we convert them as `repo-> jobName-> Presubmit` to be able to
// access any specific elements of the Presubmits without the need to iterate in slices.
func getJobsByRepoAndName(presubmits config.Presubmits) map[string]map[string]prowconfig.Presubmit {
	jobsByRepo := make(map[string]map[string]prowconfig.Presubmit)

	for repo, preSubmitList := range presubmits {
		pm := make(map[string]prowconfig.Presubmit)
		for _, p := range preSubmitList {
			pm[p.Name] = p
		}
		jobsByRepo[repo] = pm
	}
	return jobsByRepo
}

// PostsubmitInContext is a postsubmit with the org/repo#branch for which it will trigger
type PostsubmitInContext struct {
	Info config.Info
	Job  prowconfig.Postsubmit
}

// GetImagesPostsubmitsForCiopConfigs determines the [images] postsubmit jobs affected by the changed
// ci-operator configurations
func GetImagesPostsubmitsForCiopConfigs(prowConfig *prowconfig.Config, ciopConfigs config.ByFilename) []PostsubmitInContext {
	var ret []PostsubmitInContext

	for _, data := range ciopConfigs {
		jobNamePrefix := data.Info.JobName(jobconfig.PostsubmitPrefix, "")
		for _, job := range prowConfig.JobConfig.PostsubmitsStatic[fmt.Sprintf("%s/%s", data.Info.Org, data.Info.Repo)] {
			if job.Agent != string(pjapi.KubernetesAgent) {
				continue
			}
			if !strings.HasPrefix(job.Name, jobNamePrefix) {
				continue
			}
			testName := strings.TrimPrefix(job.Name, jobNamePrefix)

			if testName == "images" {
				ret = append(ret, PostsubmitInContext{
					Info: data.Info,
					Job:  job,
				})
			}
		}
	}

	return ret
}

func GetPresubmitsForCiopConfigs(prowConfig *prowconfig.Config, ciopConfigs config.ByFilename, affectedJobs map[string]sets.String) config.Presubmits {
	ret := config.Presubmits{}

	for _, data := range ciopConfigs {
		orgRepo := fmt.Sprintf("%s/%s", data.Info.Org, data.Info.Repo)
		jobNamePrefix := data.Info.JobName(jobconfig.PresubmitPrefix, "")
		for _, job := range prowConfig.JobConfig.PresubmitsStatic[orgRepo] {
			if job.Agent != string(pjapi.KubernetesAgent) {
				continue
			}
			if !strings.HasPrefix(job.Name, jobNamePrefix) {
				continue
			}
			testName := strings.TrimPrefix(job.Name, jobNamePrefix)

			affectedJob, ok := affectedJobs[data.Info.Basename()]
			if ok && !affectedJob.Has(testName) {
				continue
			}

			ret.Add(orgRepo, job)
		}
	}

	return ret
}

func getTestsByName(tests []cioperatorapi.TestStepConfiguration) map[string]cioperatorapi.TestStepConfiguration {
	ret := make(map[string]cioperatorapi.TestStepConfiguration)
	for _, test := range tests {
		ret[test.As] = test
	}
	return ret
}

// GetPresubmitsForClusterProfiles returns a filtered list of jobs from the
// Prow configuration, with only presubmits that use certain cluster profiles.
func GetPresubmitsForClusterProfiles(prowConfig *prowconfig.Config, profiles []config.ConfigMapSource) config.Presubmits {
	names := make(sets.String, len(profiles))
	for _, p := range profiles {
		names.Insert(p.CMName(config.ClusterProfilePrefix))
	}
	matches := func(job *prowconfig.Presubmit) bool {
		if job.Agent != string(pjapi.KubernetesAgent) {
			return false
		}
		for _, v := range job.Spec.Volumes {
			if v.Name != "cluster-profile" || v.Projected == nil {
				continue
			}
			for _, s := range v.Projected.Sources {
				if s.ConfigMap != nil && names.Has(s.ConfigMap.Name) {
					return true
				}
			}
		}
		return false
	}
	ret := config.Presubmits{}
	for repo, jobs := range prowConfig.JobConfig.PresubmitsStatic {
		for _, job := range jobs {
			if matches(&job) {
				ret.Add(repo, job)
			}
		}
	}
	return ret
}

// GetChangedPeriodics compares the periodic jobs from two prow configs and returns a list the changed periodics.
func GetChangedPeriodics(prowMasterConfig, prowPRConfig *prowconfig.Config, logger *logrus.Entry) config.Periodics {
	changedPeriodics := config.Periodics{}
	masterPeriodicsPerName := getPeriodicsPerName(prowMasterConfig.JobConfig.AllPeriodics())

	for name, job := range getPeriodicsPerName(prowPRConfig.JobConfig.AllPeriodics()) {
		if job.Agent == string(pjapi.KubernetesAgent) {
			masterPeriodics := masterPeriodicsPerName[name]
			if !equality.Semantic.DeepEqual(masterPeriodics.Spec, job.Spec) {
				logger.WithFields(logrus.Fields{logJobName: job.Name}).Info(chosenJob)
				changedPeriodics[job.Name] = job
			} else if masterPeriodics.Cluster != job.Cluster {
				logger.WithFields(logrus.Fields{logJobName: job.Name}).Info(chosenJob)
				changedPeriodics[job.Name] = job
			}
		}
	}

	return changedPeriodics
}

func getPeriodicsPerName(periodics []prowconfig.Periodic) map[string]prowconfig.Periodic {
	ret := make(map[string]prowconfig.Periodic, len(periodics))
	for _, periodic := range periodics {
		ret[periodic.Name] = periodic
	}
	return ret
}
