// This file is part of arduino-cli.
//
// Copyright 2020 ARDUINO SA (http://www.arduino.cc/)
//
// This software is released under the GNU General Public License version 3,
// which covers the main part of arduino-cli.
// The terms of this license can be found at:
// https://www.gnu.org/licenses/gpl-3.0.en.html
//
// You can be released from the requirements of the above licenses by purchasing
// a commercial license. Buying such a license is mandatory if you want to
// modify or otherwise use the software for commercial activities involving the
// Arduino software without disclosing the source code of your own applications.
// To purchase a commercial license, send an email to license@arduino.cc.

package commands

import (
	"context"
	"errors"
	"fmt"

	"github.com/arduino/arduino-cli/commands/cmderrors"
	"github.com/arduino/arduino-cli/commands/internal/instances"
	"github.com/arduino/arduino-cli/internal/arduino/libraries"
	"github.com/arduino/arduino-cli/internal/arduino/libraries/librariesindex"
	"github.com/arduino/arduino-cli/internal/arduino/libraries/librariesmanager"
	rpc "github.com/arduino/arduino-cli/rpc/cc/arduino/cli/commands/v1"
	"github.com/arduino/go-paths-helper"
	"github.com/sirupsen/logrus"
)

// LibraryInstall resolves the library dependencies, then downloads and installs the libraries into the install location.
func LibraryInstall(ctx context.Context, req *rpc.LibraryInstallRequest, downloadCB rpc.DownloadProgressCB, taskCB rpc.TaskProgressCB) error {
	// Obtain the library index from the manager
	li, err := instances.GetLibrariesIndex(req.GetInstance())
	if err != nil {
		return err
	}

	toInstall := map[string]*rpc.LibraryDependencyStatus{}
	if req.GetNoDeps() {
		toInstall[req.GetName()] = &rpc.LibraryDependencyStatus{
			Name:            req.GetName(),
			VersionRequired: req.GetVersion(),
		}
	} else {
		// Obtain the library explorer from the instance
		lme, releaseLme, err := instances.GetLibraryManagerExplorer(req.GetInstance())
		if err != nil {
			return err
		}

		res, err := libraryResolveDependencies(lme, li, req.GetName(), req.GetVersion(), req.GetNoOverwrite())
		releaseLme()
		if err != nil {
			return err
		}

		for _, dep := range res.GetDependencies() {
			if existingDep, has := toInstall[dep.GetName()]; has {
				if existingDep.GetVersionRequired() != dep.GetVersionRequired() {
					err := errors.New(
						tr("two different versions of the library %[1]s are required: %[2]s and %[3]s",
							dep.GetName(), dep.GetVersionRequired(), existingDep.GetVersionRequired()))
					return &cmderrors.LibraryDependenciesResolutionFailedError{Cause: err}
				}
			}
			toInstall[dep.GetName()] = dep
		}
	}

	// Obtain the download directory
	var downloadsDir *paths.Path
	if pme, releasePme, err := instances.GetPackageManagerExplorer(req.GetInstance()); err != nil {
		return err
	} else {
		downloadsDir = pme.DownloadDir
		releasePme()
	}

	// Obtain the library installer from the manager
	lmi, releaseLmi, err := instances.GetLibraryManagerInstaller(req.GetInstance())
	if err != nil {
		return err
	}
	defer releaseLmi()

	// Find the libReleasesToInstall to install
	libReleasesToInstall := map[*librariesindex.Release]*librariesmanager.LibraryInstallPlan{}
	installLocation := libraries.FromRPCLibraryInstallLocation(req.GetInstallLocation())
	for _, lib := range toInstall {
		version, err := ParseVersion(lib.GetVersionRequired())
		if err != nil {
			return err
		}
		libRelease, err := li.FindRelease(lib.GetName(), version)
		if err != nil {
			return err
		}

		installTask, err := lmi.InstallPrerequisiteCheck(libRelease.Library.Name, libRelease.Version, installLocation)
		if err != nil {
			return err
		}
		if installTask.UpToDate {
			taskCB(&rpc.TaskProgress{Message: tr("Already installed %s", libRelease), Completed: true})
			continue
		}

		if req.GetNoOverwrite() {
			if installTask.ReplacedLib != nil {
				return fmt.Errorf(tr("Library %[1]s is already installed, but with a different version: %[2]s", libRelease, installTask.ReplacedLib))
			}
		}
		libReleasesToInstall[libRelease] = installTask
	}

	for libRelease, installTask := range libReleasesToInstall {
		// Checks if libRelease is the requested library and not a dependency
		downloadReason := "depends"
		if libRelease.GetName() == req.GetName() {
			downloadReason = "install"
			if installTask.ReplacedLib != nil {
				downloadReason = "upgrade"
			}
			if installLocation == libraries.IDEBuiltIn {
				downloadReason += "-builtin"
			}
		}
		if err := downloadLibrary(downloadsDir, libRelease, downloadCB, taskCB, downloadReason); err != nil {
			return err
		}
		if err := installLibrary(lmi, downloadsDir, libRelease, installTask, taskCB); err != nil {
			return err
		}
	}

	if err := Init(&rpc.InitRequest{Instance: req.GetInstance()}, nil); err != nil {
		return err
	}

	return nil
}

func installLibrary(lmi *librariesmanager.Installer, downloadsDir *paths.Path, libRelease *librariesindex.Release, installTask *librariesmanager.LibraryInstallPlan, taskCB rpc.TaskProgressCB) error {
	taskCB(&rpc.TaskProgress{Name: tr("Installing %s", libRelease)})
	logrus.WithField("library", libRelease).Info("Installing library")

	if libReplaced := installTask.ReplacedLib; libReplaced != nil {
		taskCB(&rpc.TaskProgress{Message: tr("Replacing %[1]s with %[2]s", libReplaced, libRelease)})
		if err := lmi.Uninstall(libReplaced); err != nil {
			return &cmderrors.FailedLibraryInstallError{
				Cause: fmt.Errorf("%s: %s", tr("could not remove old library"), err)}
		}
	}

	installPath := installTask.TargetPath
	tmpDirPath := installPath.Parent()
	if err := libRelease.Resource.Install(downloadsDir, tmpDirPath, installPath); err != nil {
		return &cmderrors.FailedLibraryInstallError{Cause: err}
	}

	taskCB(&rpc.TaskProgress{Message: tr("Installed %s", libRelease), Completed: true})
	return nil
}

// ZipLibraryInstall FIXMEDOC
func ZipLibraryInstall(ctx context.Context, req *rpc.ZipLibraryInstallRequest, taskCB rpc.TaskProgressCB) error {
	lm, err := instances.GetLibraryManager(req.GetInstance())
	if err != nil {
		return err
	}
	lmi, release := lm.NewInstaller()
	defer release()
	if err := lmi.InstallZipLib(ctx, paths.New(req.GetPath()), req.GetOverwrite()); err != nil {
		return &cmderrors.FailedLibraryInstallError{Cause: err}
	}
	taskCB(&rpc.TaskProgress{Message: tr("Library installed"), Completed: true})
	return nil
}

// GitLibraryInstall FIXMEDOC
func GitLibraryInstall(ctx context.Context, req *rpc.GitLibraryInstallRequest, taskCB rpc.TaskProgressCB) error {
	lm, err := instances.GetLibraryManager(req.GetInstance())
	if err != nil {
		return err
	}
	lmi, release := lm.NewInstaller()
	defer release()
	if err := lmi.InstallGitLib(req.GetUrl(), req.GetOverwrite()); err != nil {
		return &cmderrors.FailedLibraryInstallError{Cause: err}
	}
	taskCB(&rpc.TaskProgress{Message: tr("Library installed"), Completed: true})
	return nil
}
