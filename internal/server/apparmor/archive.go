package apparmor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/lxc/incus/v6/internal/server/sys"
	internalUtil "github.com/lxc/incus/v6/internal/util"
	"github.com/lxc/incus/v6/shared/revert"
)

// ArchiveWrapper is used as a RunWrapper in the rsync package.
func ArchiveWrapper(sysOS *sys.OS, cmd *exec.Cmd, output string, allowedCmds []string) (func(), error) {
	if !sysOS.AppArmorAvailable {
		return func() {}, nil
	}

	reverter := revert.New()
	defer reverter.Fail()

	// Load the profile.
	profileName, err := archiveProfileLoad(sysOS, output, allowedCmds)
	if err != nil {
		return nil, fmt.Errorf("Failed to load apparmor profile: %w", err)
	}

	reverter.Add(func() { _ = deleteProfile(sysOS, profileName, profileName) })

	// Resolve aa-exec.
	execPath, err := exec.LookPath("aa-exec")
	if err != nil {
		return nil, err
	}

	// Override the command.
	newArgs := []string{"aa-exec", "-p", profileName}
	newArgs = append(newArgs, cmd.Args...)
	cmd.Args = newArgs
	cmd.Path = execPath

	// All done, setup a cleanup function and disarm reverter.
	cleanup := func() {
		_ = deleteProfile(sysOS, profileName, profileName)
	}

	reverter.Success()

	return cleanup, nil
}

func archiveProfileLoad(sysOS *sys.OS, output string, allowedCommandPaths []string) (string, error) {
	reverter := revert.New()
	defer reverter.Fail()

	// Generate a temporary profile name.
	name := profileName("archive", uuid.New().String())
	profilePath := filepath.Join(aaPath, "profiles", name)

	// Generate the profile
	content, err := archiveProfile(name, output, allowedCommandPaths)
	if err != nil {
		return "", err
	}

	// Write it to disk.
	err = os.WriteFile(profilePath, []byte(content), 0o600)
	if err != nil {
		return "", err
	}

	reverter.Add(func() { os.Remove(profilePath) })

	// Load it.
	err = loadProfile(sysOS, name)
	if err != nil {
		return "", err
	}

	reverter.Success()
	return name, nil
}

// archiveProfile generates the AppArmor profile template from the given destination path.
func archiveProfile(name string, outputPath string, allowedCommandPaths []string) (string, error) {
	// Attempt to deref all paths.
	outputPathFull, err := filepath.EvalSymlinks(outputPath)
	if err != nil {
		outputPathFull = outputPath // Use requested path if cannot resolve it.
	}

	backupsPath := internalUtil.VarPath("backups")
	backupsPathFull, err := filepath.EvalSymlinks(backupsPath)
	if err == nil {
		backupsPath = backupsPathFull
	}

	imagesPath := internalUtil.VarPath("images")
	imagesPathFull, err := filepath.EvalSymlinks(imagesPath)
	if err == nil {
		imagesPath = imagesPathFull
	}

	derefCommandPaths := make([]string, len(allowedCommandPaths))
	for i, cmd := range allowedCommandPaths {
		cmdPath, err := exec.LookPath(cmd)
		if err == nil {
			cmd = cmdPath
		}

		cmdFull, err := filepath.EvalSymlinks(cmd)
		if err == nil {
			derefCommandPaths[i] = cmdFull
		} else {
			derefCommandPaths[i] = cmd
		}
	}

	// Render the profile.
	var sb *strings.Builder = &strings.Builder{}
	err = archiveProfileTpl.Execute(sb, map[string]any{
		"name":                name,
		"outputPath":          outputPathFull, // Use deferenced path in AppArmor profile.
		"backupsPath":         backupsPath,
		"imagesPath":          imagesPath,
		"allowedCommandPaths": derefCommandPaths,
	})
	if err != nil {
		return "", err
	}

	return sb.String(), nil
}
