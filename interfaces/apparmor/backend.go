// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2016 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

// Package apparmor implements integration between snappy and
// ubuntu-core-launcher around apparmor.
//
// Snappy creates apparmor profiles for each application (for each snap)
// present in the system.  Upon each execution of ubuntu-core-launcher
// application process is launched under the profile. Prior to that the profile
// must be parsed, compiled and loaded into the kernel using the support tool
// "apparmor_parser".
//
// Each apparmor profile contains a simple <header><content><footer> structure.
// The header specifies the profile name that the launcher will use to launch a
// process under this profile.  Snappy uses "abstract identifiers" as profile
// names.
//
// The actual profiles are stored in /var/lib/snappy/apparmor/profiles.
//
// NOTE: A systemd job (apparmor.service) loads all snappy-specific apparmor
// profiles into the kernel during the boot process.
package apparmor

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/release"
	"github.com/snapcore/snapd/snap"
)

var (
	procSelfExe = "/proc/self/exe"
)

// Backend is responsible for maintaining apparmor profiles for ubuntu-core-launcher.
type Backend struct{}

// Name returns the name of the backend.
func (b *Backend) Name() interfaces.SecuritySystem {
	return interfaces.SecurityAppArmor
}

// Initialize prepares customized apparmor policy for snap-confine.
func (b *Backend) Initialize() error {
	// NOTE: It would be nice if we could also generate the profile for
	// snap-confine executing from the core snap, right here, and not have to
	// do this in the Setup function below. I sadly don't think this is
	// possible because snapd must be able to install a new core and only at
	// that moment generate it.

	// Inspect the system and sets up local apparmor policy for snap-confine.
	// Local policy is included by the system-wide policy. If the local policy
	// has changed then the apparmor profile for snap-confine is reloaded.

	// Create the local policy directory if it is not there.
	if err := os.MkdirAll(dirs.SnapConfineAppArmorDir, 0755); err != nil {
		return fmt.Errorf("cannot create snap-confine policy directory: %s", err)
	}

	// Check the /proc/self/exe symlink, this is needed below but we want to
	// fail early if this fails for whatever reason.
	exe, err := os.Readlink(procSelfExe)
	if err != nil {
		return fmt.Errorf("cannot read %s: %s", procSelfExe, err)
	}

	// Location of the generated policy.
	glob := "*"
	policy := make(map[string]*osutil.FileState)

	// Check if NFS is mounted at or under $HOME. Because NFS is not
	// transparent to apparmor we must alter our profile to counter that and
	// allow snap-confine to work.
	if nfs, err := osutil.IsHomeUsingNFS(); err != nil {
		logger.Noticef("cannot determine if NFS is in use: %v", err)
	} else if nfs {
		policy["nfs-support"] = &osutil.FileState{
			Content: []byte(nfsSnippet),
			Mode:    0644,
		}
		logger.Noticef("snapd enabled NFS support, additional implicit network permissions granted")
	}

	// Ensure that generated policy is what we computed above.
	created, removed, err := osutil.EnsureDirState(dirs.SnapConfineAppArmorDir, glob, policy)
	if err != nil {
		return fmt.Errorf("cannot synchronize snap-confine policy: %s", err)
	}
	if len(created) == 0 && len(removed) == 0 {
		// If the generated policy didn't change, we're all done.
		return nil
	}

	// If snapd is executing from the core snap the it means it has
	// re-executed. In that case we are no longer using the copy of
	// snap-confine from the host distribution but our own copy. We don't have
	// to re-compile and load the updated profile as that is performed by
	// setupSnapConfineReexec below.
	if strings.HasPrefix(exe, dirs.SnapMountDir) {
		return nil
	}

	// Reload the apparmor profile of snap-confine. This points to the main
	// file in /etc/apparmor.d/ as that file contains include statements that
	// load any of the files placed in /var/lib/snapd/apparmor/snap-confine/.
	// For historical reasons we may have a filename that ends with .real or
	// not.  If we do then we prefer the file ending with the name .real as
	// that is the more recent name we use.
	var profilePath string
	for _, profileFname := range []string{"usr.lib.snapd.snap-confine.real", "usr.lib.snapd.snap-confine"} {
		profilePath = filepath.Join(dirs.SystemApparmorDir, profileFname)
		if _, err := os.Stat(profilePath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		break
	}
	if profilePath == "" {
		return fmt.Errorf("cannot find system apparmor profile for snap-confine")
	}

	// We are not using apparmor.LoadProfile() because it uses other cache.
	cmd := exec.Command("apparmor_parser", "--replace",
		// Use no-expr-simplify since expr-simplify is actually slower on armhf (LP: #1383858)
		"-O", "no-expr-simplify",
		"--write-cache", "--cache-loc", dirs.SystemApparmorCacheDir,
		profilePath)

	if output, err := cmd.CombinedOutput(); err != nil {
		// When we cannot reload the profile then let's remove the generated
		// policy. Maybe we have caused the problem so it's better to let other
		// things work.
		osutil.EnsureDirState(dirs.SnapConfineAppArmorDir, glob, nil)
		return fmt.Errorf("cannot reload snap-confine apparmor profile: %v", osutil.OutputErr(output, err))
	}
	return nil
}

// snapConfineFromCoreProfile returns the apparmor profile for snap-confine in the given core snap.
func snapConfineFromCoreProfile(coreInfo *snap.Info) (dir, glob string, content map[string]*osutil.FileState, err error) {
	// Find the vanilla apparmor profile for snap-confine as present in the given core snap.

	// We must test the ".real" suffix first, this is a workaround for
	// https://bugs.debian.org/cgi-bin/bugreport.cgi?bug=858004
	vanillaProfilePath := filepath.Join(coreInfo.MountDir(), "/etc/apparmor.d/usr.lib.snapd.snap-confine.real")
	vanillaProfileText, err := ioutil.ReadFile(vanillaProfilePath)
	if os.IsNotExist(err) {
		vanillaProfilePath = filepath.Join(coreInfo.MountDir(), "/etc/apparmor.d/usr.lib.snapd.snap-confine")
		vanillaProfileText, err = ioutil.ReadFile(vanillaProfilePath)
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("cannot open apparmor profile for vanilla snap-confine: %s", err)
	}

	// Replace the path to vanilla snap-confine with the path to the mounted snap-confine from core.
	snapConfineInCore := filepath.Join(coreInfo.MountDir(), "usr/lib/snapd/snap-confine")
	patchedProfileText := bytes.Replace(
		vanillaProfileText, []byte("/usr/lib/snapd/snap-confine"), []byte(snapConfineInCore), -1)
	patchedProfileName := strings.Replace(snapConfineInCore[1:], "/", ".", -1)
	patchedProfileGlob := strings.Replace(patchedProfileName, coreInfo.Revision.String(), "*", 1)

	// Return information for EnsureDirState that describes the re-exec profile for snap-confine.
	content = map[string]*osutil.FileState{
		patchedProfileName: {
			Content: []byte(patchedProfileText),
			Mode:    0644,
		},
	}
	return dirs.SystemApparmorDir, patchedProfileGlob, content, nil
}

// setupSnapConfineReexec will setup apparmor profiles on a classic
// system on the hosts /etc/apparmor.d directory. This is needed for
// running snap-confine from the core snap.
//
// Additionally it will cleanup stale apparmor profiles it created.
func setupSnapConfineReexec(coreInfo *snap.Info) error {
	err := os.MkdirAll(dirs.SnapConfineAppArmorDir, 0755)
	if err != nil {
		return fmt.Errorf("cannot create snap-confine policy directory: %s", err)
	}

	dir, glob, content, err := snapConfineFromCoreProfile(coreInfo)
	cache := dirs.SystemApparmorCacheDir
	if err != nil {
		return fmt.Errorf("cannot compute snap-confine profile: %s", err)
	}

	changed, removed, errEnsure := osutil.EnsureDirState(dir, glob, content)
	if len(changed) == 0 {
		// XXX: because NFS workaround is handled separately the same correct
		// snap-confine profile may need to be re-loaded. This is because the
		// profile contains include directives and those load a second file
		// that has changed outside of the scope of EnsureDirState.
		//
		// To counter that, always reload the profile by pretending it had
		// changed.
		for fname := range content {
			changed = append(changed, fname)
		}
	}
	errReload := reloadProfiles(changed, dir, cache)
	errUnload := unloadProfiles(removed, cache)
	if errEnsure != nil {
		return fmt.Errorf("cannot synchronize snap-confine apparmor profile: %s", errEnsure)
	}
	if errReload != nil {
		return fmt.Errorf("cannot reload snap-confine apparmor profile: %s", errReload)
	}
	if errUnload != nil {
		return fmt.Errorf("cannot unload snap-confine apparmor profile: %s", errReload)
	}
	return nil
}

// Setup creates and loads apparmor profiles specific to a given snap.
// The snap can be in developer mode to make security violations non-fatal to
// the offending application process.
//
// This method should be called after changing plug, slots, connections between
// them or application present in the snap.
func (b *Backend) Setup(snapInfo *snap.Info, opts interfaces.ConfinementOptions, repo *interfaces.Repository) error {
	snapName := snapInfo.Name()
	spec, err := repo.SnapSpecification(b.Name(), snapName)
	if err != nil {
		return fmt.Errorf("cannot obtain apparmor specification for snap %q: %s", snapName, err)
	}
	spec.(*Specification).AddSnapLayout(snapInfo)

	// core on classic is special
	if snapName == "core" && release.OnClassic && release.AppArmorLevel() != release.NoAppArmor {
		if err := setupSnapConfineReexec(snapInfo); err != nil {
			logger.Noticef("cannot create host snap-confine apparmor configuration: %s", err)
		}
	}
	// core on core devices is also special, the apparmor cache gets
	// confused too easy, especially at rollbacks, so we delete the cache.
	// See LP:#1460152 and
	// https://forum.snapcraft.io/t/core-snap-revert-issues-on-core-devices/
	if snapName == "core" && !release.OnClassic {
		if li, err := filepath.Glob(filepath.Join(dirs.SystemApparmorCacheDir, "*")); err == nil {
			for _, p := range li {
				if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
					if err := os.Remove(p); err != nil {
						logger.Noticef("cannot remove %q: %s", p, err)
					}
				}
			}
		}
	}

	// Get the files that this snap should have
	content, err := b.deriveContent(spec.(*Specification), snapInfo, opts)
	if err != nil {
		return fmt.Errorf("cannot obtain expected security files for snap %q: %s", snapName, err)
	}
	dir := dirs.SnapAppArmorDir
	glob := interfaces.SecurityTagGlob(snapInfo.Name())
	cache := dirs.AppArmorCacheDir
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create directory for apparmor profiles %q: %s", dir, err)
	}
	_, removed, errEnsure := osutil.EnsureDirState(dir, glob, content)
	// NOTE: load all profiles instead of just the changed profiles.  We're
	// relying on apparmor cache to make this efficient. This gives us
	// certainty that each call to Setup ends up with working profiles.
	all := make([]string, 0, len(content))
	for name := range content {
		all = append(all, name)
	}
	sort.Strings(all)
	errReload := reloadProfiles(all, dir, cache)
	errUnload := unloadProfiles(removed, cache)
	if errEnsure != nil {
		return fmt.Errorf("cannot synchronize security files for snap %q: %s", snapName, errEnsure)
	}
	if errReload != nil {
		return errReload
	}
	return errUnload
}

// Remove removes and unloads apparmor profiles of a given snap.
func (b *Backend) Remove(snapName string) error {
	dir := dirs.SnapAppArmorDir
	glob := fmt.Sprintf("snap*.%s*", snapName)
	cache := dirs.AppArmorCacheDir
	_, removed, errEnsure := osutil.EnsureDirState(dir, glob, nil)
	errUnload := unloadProfiles(removed, cache)
	if errEnsure != nil {
		return fmt.Errorf("cannot synchronize security files for snap %q: %s", snapName, errEnsure)
	}
	return errUnload
}

var (
	templatePattern = regexp.MustCompile("(###[A-Z]+###)")
	attachPattern   = regexp.MustCompile(`\(attach_disconnected\)`)
)

const attachComplain = "(attach_disconnected,complain)"

func (b *Backend) deriveContent(spec *Specification, snapInfo *snap.Info, opts interfaces.ConfinementOptions) (content map[string]*osutil.FileState, err error) {
	for _, appInfo := range snapInfo.Apps {
		if content == nil {
			content = make(map[string]*osutil.FileState)
		}
		securityTag := appInfo.SecurityTag()
		addContent(securityTag, snapInfo, opts, spec.SnippetForTag(securityTag), content)
	}

	for _, hookInfo := range snapInfo.Hooks {
		if content == nil {
			content = make(map[string]*osutil.FileState)
		}
		securityTag := hookInfo.SecurityTag()
		addContent(securityTag, snapInfo, opts, spec.SnippetForTag(securityTag), content)
	}

	return content, nil
}

func addContent(securityTag string, snapInfo *snap.Info, opts interfaces.ConfinementOptions, snippetForTag string, content map[string]*osutil.FileState) {
	var policy string
	// When partial AppArmor is detected, use the classic template for now. We could
	// use devmode, but that could generate confusing log entries for users running
	// snaps on systems with partial AppArmor support.
	level := release.AppArmorLevel()
	if level == release.PartialAppArmor || (opts.Classic && !opts.JailMode) {
		policy = classicTemplate
	} else {
		policy = defaultTemplate
	}
	if (opts.DevMode || opts.Classic) && !opts.JailMode {
		policy = attachPattern.ReplaceAllString(policy, attachComplain)
	}
	policy = templatePattern.ReplaceAllStringFunc(policy, func(placeholder string) string {
		switch placeholder {
		case "###VAR###":
			return templateVariables(snapInfo, securityTag)
		case "###PROFILEATTACH###":
			return fmt.Sprintf("profile \"%s\"", securityTag)
		case "###SNIPPETS###":
			var tagSnippets string
			if opts.Classic && opts.JailMode {
				// Add a special internal snippet for snaps using classic confinement
				// and jailmode together. This snippet provides access to the core snap
				// so that the dynamic linker and shared libraries can be used.
				tagSnippets = classicJailmodeSnippet + "\n" + snippetForTag
			} else if level == release.PartialAppArmor || (opts.Classic && !opts.JailMode) {
				// When classic confinement (without jailmode) is in effect we
				// are ignoring all apparmor snippets as they may conflict with
				// the super-broad template we are starting with.
			} else {
				// Check if NFS is mounted at or under $HOME. Because NFS is not
				// transparent to apparmor we must alter the profile to counter that and
				// allow access to SNAP_USER_* files.
				tagSnippets = snippetForTag
				if nfs, _ := osutil.IsHomeUsingNFS(); nfs {
					tagSnippets += nfsSnippet
				}
			}
			return tagSnippets
		}
		return ""
	})

	content[securityTag] = &osutil.FileState{
		Content: []byte(policy),
		Mode:    0644,
	}
}

func reloadProfiles(profiles []string, profileDir, cacheDir string) error {
	for _, profile := range profiles {
		err := loadProfile(filepath.Join(profileDir, profile), cacheDir)
		if err != nil {
			return fmt.Errorf("cannot load apparmor profile %q: %s", profile, err)
		}
	}
	return nil
}

func unloadProfiles(profiles []string, cacheDir string) error {
	for _, profile := range profiles {
		if err := unloadProfile(profile, cacheDir); err != nil {
			return fmt.Errorf("cannot unload apparmor profile %q: %s", profile, err)
		}
	}
	return nil
}

// NewSpecification returns a new, empty apparmor specification.
func (b *Backend) NewSpecification() interfaces.Specification {
	return &Specification{}
}
