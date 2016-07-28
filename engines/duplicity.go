package engines

import (
	"errors"
	"fmt"
	"io/ioutil"
	"regexp"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/camptocamp/conplicity/handler"
	"github.com/camptocamp/conplicity/util"
	"github.com/camptocamp/conplicity/volume"
	"github.com/docker/engine-api/types"
	"github.com/docker/engine-api/types/container"
	"golang.org/x/net/context"
)

// DuplicityEngine implements a backup engine with Duplicity
type DuplicityEngine struct {
	Handler *handler.Conplicity
	Volume  *volume.Volume
}

// Constants
const cacheMount = "duplicity_cache:/root/.cache/duplicity"
const timeFormat = "Mon Jan 2 15:04:05 2006"

var fullBackupRx = regexp.MustCompile("Last full backup date: (.+)")
var chainEndTimeRx = regexp.MustCompile("Chain end time: (.+)")

// GetName returns the engine name
func (*DuplicityEngine) GetName() string {
	return "Duplicity"
}

// Backup performs the backup of the passed volume
func (d *DuplicityEngine) Backup() (metrics []string, err error) {
	vol := d.Volume
	log.WithFields(log.Fields{
		"volume":     vol.Name,
		"driver":     vol.Driver,
		"mountpoint": vol.Mountpoint,
	}).Info("Creating duplicity container")

	fullIfOlderThan, _ := util.GetVolumeLabel(vol.Volume, ".full_if_older_than")
	if fullIfOlderThan == "" {
		fullIfOlderThan = d.Handler.Config.Duplicity.FullIfOlderThan
	}

	removeOlderThan, _ := util.GetVolumeLabel(vol.Volume, ".remove_older_than")
	if removeOlderThan == "" {
		removeOlderThan = d.Handler.Config.Duplicity.RemoveOlderThan
	}

	pathSeparator := "/"
	if strings.HasPrefix(d.Handler.Config.Duplicity.TargetURL, "swift://") {
		// Looks like I'm not the one to fall on this issue: http://stackoverflow.com/questions/27991960/upload-to-swift-pseudo-folders-using-duplicity
		pathSeparator = "_"
	}

	backupDir := vol.BackupDir
	vol.Target = d.Handler.Config.Duplicity.TargetURL + pathSeparator + d.Handler.Hostname + pathSeparator + vol.Name
	vol.BackupDir = vol.Mountpoint + "/" + backupDir
	vol.Mount = vol.Name + ":" + vol.Mountpoint + ":ro"
	vol.FullIfOlderThan = fullIfOlderThan
	vol.RemoveOlderThan = removeOlderThan

	var newMetrics []string

	newMetrics, err = d.duplicityBackup()
	util.CheckErr(err, "Failed to backup volume "+vol.Name+" : %v", "fatal")
	metrics = append(metrics, newMetrics...)

	_, err = d.removeOld()
	util.CheckErr(err, "Failed to remove old backups for volume "+vol.Name+" : %v", "fatal")

	_, err = d.cleanup()
	util.CheckErr(err, "Failed to cleanup extraneous duplicity files for volume "+vol.Name+" : %v", "fatal")

	noVerifyLbl, _ := util.GetVolumeLabel(vol.Volume, ".no_verify")
	noVerify := d.Handler.Config.NoVerify || (noVerifyLbl == "true")
	if noVerify {
		log.WithFields(log.Fields{
			"volume": vol.Name,
		}).Info("Skipping verification")
	} else {
		newMetrics, err = d.verify()
		util.CheckErr(err, "Failed to verify backup for volume "+vol.Name+" : %v", "fatal")
		metrics = append(metrics, newMetrics...)
	}

	newMetrics, err = d.status()
	util.CheckErr(err, "Failed to retrieve last backup info for volume "+vol.Name+" : %v", "fatal")
	metrics = append(metrics, newMetrics...)

	return
}

// removeOld cleans up old backup data
func (d *DuplicityEngine) removeOld() (metrics []string, err error) {
	v := d.Volume
	_, _, err = d.launchDuplicity(
		[]string{
			"remove-older-than", v.RemoveOlderThan,
			"--s3-use-new-style",
			"--ssh-options", "-oStrictHostKeyChecking=no",
			"--no-encryption",
			"--force",
			"--name", v.Name,
			v.Target,
		},
		[]string{
			cacheMount,
		},
	)
	util.CheckErr(err, "Failed to launch Duplicity: %v", "fatal")
	return
}

// cleanup removes old index data from duplicity
func (d *DuplicityEngine) cleanup() (metrics []string, err error) {
	v := d.Volume
	_, _, err = d.launchDuplicity(
		[]string{
			"cleanup",
			"--s3-use-new-style",
			"--ssh-options", "-oStrictHostKeyChecking=no",
			"--no-encryption",
			"--force",
			"--extra-clean",
			"--name", v.Name,
			v.Target,
		},
		[]string{
			cacheMount,
		},
	)
	util.CheckErr(err, "Failed to launch Duplicity: %v", "fatal")
	return
}

// verify checks that the backup is usable
func (d *DuplicityEngine) verify() (metrics []string, err error) {
	v := d.Volume
	state, _, err := d.launchDuplicity(
		[]string{
			"verify",
			"--s3-use-new-style",
			"--ssh-options", "-oStrictHostKeyChecking=no",
			"--no-encryption",
			"--allow-source-mismatch",
			"--name", v.Name,
			v.Target,
			v.BackupDir,
		},
		[]string{
			v.Mount,
			cacheMount,
		},
	)
	util.CheckErr(err, "Failed to launch Duplicity: %v", "fatal")

	metric := fmt.Sprintf("conplicity{volume=\"%v\",what=\"verifyExitCode\"} %v", v.Name, state)
	metrics = []string{
		metric,
	}
	return
}

// status gets the latest backup date info from duplicity
func (d *DuplicityEngine) status() (metrics []string, err error) {
	v := d.Volume
	_, stdout, err := d.launchDuplicity(
		[]string{
			"collection-status",
			"--s3-use-new-style",
			"--ssh-options", "-oStrictHostKeyChecking=no",
			"--no-encryption",
			"--name", v.Name,
			v.Target,
		},
		[]string{
			v.Mount,
			cacheMount,
		},
	)
	util.CheckErr(err, "Failed to launch Duplicity: %v", "fatal")

	fullBackup := fullBackupRx.FindStringSubmatch(stdout)
	var fullBackupDate time.Time
	chainEndTime := chainEndTimeRx.FindStringSubmatch(stdout)
	var chainEndTimeDate time.Time

	if len(fullBackup) > 0 {
		if strings.TrimSpace(fullBackup[1]) == "none" {
			fullBackupDate = time.Unix(0, 0)
			chainEndTimeDate = time.Unix(0, 0)
		} else {
			fullBackupDate, err = time.Parse(timeFormat, strings.TrimSpace(fullBackup[1]))
			util.CheckErr(err, "Failed to parse full backup date: %v", "error")

			if len(chainEndTime) > 0 {
				chainEndTimeDate, err = time.Parse(timeFormat, strings.TrimSpace(chainEndTime[1]))
				util.CheckErr(err, "Failed to parse chain end time date: %v", "error")
			} else {
				errMsg := fmt.Sprintf("Failed to parse Duplicity output for chain end time of %v", v.Name)
				err = errors.New(errMsg)
				return
			}

		}
	} else {
		errMsg := fmt.Sprintf("Failed to parse Duplicity output for last full backup date of %v", v.Name)
		err = errors.New(errMsg)
		return
	}

	lastBackupMetric := fmt.Sprintf("conplicity{volume=\"%v\",what=\"lastBackup\"} %v", v.Name, chainEndTimeDate.Unix())

	lastFullBackupMetric := fmt.Sprintf("conplicity{volume=\"%v\",what=\"lastFullBackup\"} %v", v.Name, fullBackupDate.Unix())

	metrics = []string{
		lastBackupMetric,
		lastFullBackupMetric,
	}

	return
}

// launchDuplicity starts a duplicity container with given command and binds
func (d *DuplicityEngine) launchDuplicity(cmd []string, binds []string) (state int, stdout string, err error) {
	util.PullImage(d.Handler.Client, d.Handler.Config.Duplicity.Image)
	util.CheckErr(err, "Failed to pull image: %v", "fatal")

	env := []string{
		"AWS_ACCESS_KEY_ID=" + d.Handler.Config.AWS.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY=" + d.Handler.Config.AWS.SecretAccessKey,
		"SWIFT_USERNAME=" + d.Handler.Config.Swift.Username,
		"SWIFT_PASSWORD=" + d.Handler.Config.Swift.Password,
		"SWIFT_AUTHURL=" + d.Handler.Config.Swift.AuthURL,
		"SWIFT_TENANTNAME=" + d.Handler.Config.Swift.TenantName,
		"SWIFT_REGIONNAME=" + d.Handler.Config.Swift.RegionName,
		"SWIFT_AUTHVERSION=2",
	}

	log.WithFields(log.Fields{
		"image":       d.Handler.Config.Duplicity.Image,
		"command":     strings.Join(cmd, " "),
		"environment": strings.Join(env, ", "),
		"binds":       strings.Join(binds, ", "),
	}).Debug("Creating container")

	container, err := d.Handler.ContainerCreate(
		context.Background(),
		&container.Config{
			Cmd:          cmd,
			Env:          env,
			Image:        d.Handler.Config.Duplicity.Image,
			OpenStdin:    true,
			StdinOnce:    true,
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			Tty:          true,
		},
		&container.HostConfig{
			Binds: binds,
		}, nil, "",
	)
	util.CheckErr(err, "Failed to create container: %v", "fatal")
	defer util.RemoveContainer(d.Handler.Client, container.ID)

	log.Debugf("Launching 'duplicity %v'...", strings.Join(cmd, " "))
	err = d.Handler.ContainerStart(context.Background(), container.ID, types.ContainerStartOptions{})
	util.CheckErr(err, "Failed to start container: %v", "fatal")

	var exited bool

	for !exited {
		cont, err := d.Handler.ContainerInspect(context.Background(), container.ID)
		util.CheckErr(err, "Failed to inspect container: %v", "error")

		if cont.State.Status == "exited" {
			exited = true
			state = cont.State.ExitCode
		}
	}

	body, err := d.Handler.ContainerLogs(context.Background(), container.ID, types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Details:    true,
		Follow:     true,
	})
	util.CheckErr(err, "Failed to retrieve logs: %v", "error")

	defer body.Close()
	content, err := ioutil.ReadAll(body)
	util.CheckErr(err, "Failed to read logs from response: %v", "error")

	stdout = string(content)

	log.Debug(stdout)

	return
}

// duplicityBackup performs the backup of a volume with duplicity
func (d *DuplicityEngine) duplicityBackup() (metrics []string, err error) {
	v := d.Volume
	log.WithFields(log.Fields{
		"name":               v.Name,
		"backup_dir":         v.BackupDir,
		"full_if_older_than": v.FullIfOlderThan,
		"target":             v.Target,
		"mount":              v.Mount,
	}).Debug("Starting volume backup")

	// TODO
	// Init engine

	state, _, err := d.launchDuplicity(
		[]string{
			"--full-if-older-than", v.FullIfOlderThan,
			"--s3-use-new-style",
			"--ssh-options", "-oStrictHostKeyChecking=no",
			"--no-encryption",
			"--allow-source-mismatch",
			"--name", v.Name,
			v.BackupDir,
			v.Target,
		},
		[]string{
			v.Mount,
			cacheMount,
		},
	)
	util.CheckErr(err, "Failed to launch Duplicity: %v", "fatal")

	metric := fmt.Sprintf("conplicity{volume=\"%v\",what=\"backupExitCode\"} %v", v.Name, state)
	metrics = []string{
		metric,
	}
	return
}
