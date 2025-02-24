package drivers

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/lxc/incus/v6/internal/linux"
	"github.com/lxc/incus/v6/internal/server/operations"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/logger"
	"github.com/lxc/incus/v6/shared/revert"
	"github.com/lxc/incus/v6/shared/subprocess"
	"github.com/lxc/incus/v6/shared/units"
	"github.com/lxc/incus/v6/shared/util"
	"github.com/lxc/incus/v6/shared/validate"
	"golang.org/x/sys/unix"
)

// ContentTypeRootImg implies the filesystem contains a root.img which itself contains a filesystem
const ContentTypeFsImg = ContentType("fs-img")

// returns a clone of the img, but margked as an fs-img
func cloneVolAsFsImgVol(vol Volume) Volume {
	fsImgVol := vol.Clone()

	//fsImgVol.mountCustomPath = fmt.Sprintf("%s_%s", fsImgVol.MountPath(), fsImgVol.ConfigBlockFilesystem())
	//fsImgVol.mountCustomPath = fmt.Sprintf("%s_%s", fsImgVol.MountPath(), fsImgVol.ConfigBlockFilesystem())
	fsImgVol.mountCustomPath = fsImgVol.MountPath() + ".block"

	//fsImgVol.config["volatile.truenas.fs-img"] = "true"
	fsImgVol.contentType = ContentTypeFsImg

	return fsImgVol
}

func isFsImgVol(vol Volume) bool {
	/*
		we need a third volume type so that we can tell the difference between an
		image:fs and an image:block, adn the image:block's config mount.

		Additionally, to mount the backing image for an image:fs, we need to use a
		different contentType to obtain a separate lock, without using block (see above)
	*/
	return vol.contentType == ContentTypeFsImg

}

func needsFsImgVol(vol Volume) bool {
	/*
		does the volume need an underlying FsImgVol

		the trick is to make sure that Images etc that aren't created via NewVMBlockFilesystemVolume
		are marked as loop-vols, where as NewVMBlockFilesystemVolume must not be.

		This is accomplished by ensuring that block.filesistem is applied in FillVolumeConfig
	*/
	return vol.contentType == ContentTypeFS && vol.config["block.filesystem"] != ""
}

// CreateVolume creates an empty volume and can optionally fill it by executing the supplied
// filler function.
func (d *truenas) CreateVolume(vol Volume, filler *VolumeFiller, op *operations.Operation) error {
	/*
		incus storage volume create incusdev <name> --type=block
			name: "<project>_<name>"
			volType: "custom"		VolumeTypeCustom
			contentType: "block"	ContentTypeBlock

		incus storage volume create incusdev <name>
			name: "<project>_<name>"
			volType: "custom"		VolumeTypeCustom
			contentType: "filesystem"	ContentTypeFS

		incus storage volume create incusdev <name> zfs.block_mode=true

		incus create --empty empty2 --storage incusdev
			volType: "containers"
			contentType: "filesystem"



	*/

	// Revert handling
	revert := revert.New()
	defer revert.Fail()

	// must mount VM.block so we can access the root.img, as well as the config filesystem
	if vol.IsVMBlock() {
		vol.mountCustomPath = vol.MountPath() + ".block"
	}

	// Create mountpoint.
	err := vol.EnsureMountPath()
	if err != nil {
		return err
	}

	revert.Add(func() { _ = os.Remove(vol.MountPath()) })

	// Look for previously deleted images. (don't look for underlying, or we'll look after we've looked)
	if vol.volType == VolumeTypeImage && !isFsImgVol(vol) {
		dataset := d.dataset(vol, true)
		exists, err := d.datasetExists(dataset)
		if err != nil {
			return err
		}

		if exists {
			canRestore := true

			if vol.IsBlockBacked() && (vol.contentType == ContentTypeBlock || d.isBlockBacked(vol)) {
				// For block volumes check if the cached image volume is larger than the current pool volume.size
				// setting (if so we won't be able to resize the snapshot to that the smaller size later).
				volSize, err := d.getDatasetProperty(dataset, "volsize")
				if err != nil {
					return err
				}

				volSizeBytes, err := strconv.ParseInt(volSize, 10, 64)
				if err != nil {
					return err
				}

				poolVolSize := DefaultBlockSize
				if vol.poolConfig["volume.size"] != "" {
					poolVolSize = vol.poolConfig["volume.size"]
				}

				poolVolSizeBytes, err := units.ParseByteSizeString(poolVolSize)
				if err != nil {
					return err
				}

				// Round to block boundary.
				poolVolSizeBytes, err = d.roundVolumeBlockSizeBytes(vol, poolVolSizeBytes)
				if err != nil {
					return err
				}

				// If the cached volume size is different than the pool volume size, then we can't use the
				// deleted cached image volume and instead we will rename it to a random UUID so it can't
				// be restored in the future and a new cached image volume will be created instead.
				if volSizeBytes != poolVolSizeBytes {
					d.logger.Debug("Renaming deleted cached image volume so that regeneration is used", logger.Ctx{"fingerprint": vol.Name()})
					randomVol := NewVolume(d, d.name, vol.volType, vol.contentType, d.randomVolumeName(vol), vol.config, vol.poolConfig)

					_, err := subprocess.RunCommand("/proc/self/exe", "forkzfs", "--", "rename", dataset, d.dataset(randomVol, true))
					//_, err := d.runTool("dataset", "rename", d.dataset(vol, true), d.dataset(randomVol, true))
					if err != nil {
						return err
					}

					if vol.IsVMBlock() {
						fsVol := vol.NewVMBlockFilesystemVolume()
						randomFsVol := randomVol.NewVMBlockFilesystemVolume()

						_, err := subprocess.RunCommand("/proc/self/exe", "forkzfs", "--", "rename", d.dataset(fsVol, true), d.dataset(randomFsVol, true))
						//_, err := d.runTool("dataset", "rename", d.dataset(fsVol, true), d.dataset(randomFsVol, true))
						if err != nil {
							return err
						}
					}

					// We have renamed the deleted cached image volume, so we don't want to try and
					// restore it.
					canRestore = false
				}
			}

			// Restore the image.
			if canRestore {
				d.logger.Debug("Restoring previously deleted cached image volume", logger.Ctx{"fingerprint": vol.Name()})
				//_, err := subprocess.RunCommand("/proc/self/exe", "forkzfs", "--", "rename", d.dataset(vol, true), d.dataset(vol, false))
				_, err := d.runTool("dataset", "rename", dataset, d.dataset(vol, false))
				if err != nil {
					return err
				}

				// if vol.IsVMBlock() {
				// 	fsVol := vol.NewVMBlockFilesystemVolume()

				// 	//_, err := subprocess.RunCommand("/proc/self/exe", "forkzfs", "--", "rename", d.dataset(fsVol, true), d.dataset(fsVol, false))
				// 	_, err := d.runTool("dataset", "rename", d.dataset(fsVol, true), d.dataset(fsVol, false))

				// 	if err != nil {
				// 		return err
				// 	}
				// }

				revert.Success()
				return nil
			}
		}
	}

	// After this point we may have a volume, so setup revert.
	revert.Add(func() { _ = d.DeleteVolume(vol, op) })

	/*
		if we are creating a block_mode volume we start by creating a regular fs to host
	*/
	if needsFsImgVol(vol) { // ie create the fs-img
		/*
			by making an FS Block volume, we automatically create the root.img file and fill it out
			same as we do for a VM, which means we can now mount it too.
		*/
		fsImgVol := cloneVolAsFsImgVol(vol)

		innerFiller := &VolumeFiller{
			Fill: func(innerVol Volume, rootBlockPath string, allowUnsafeResize bool) (int64, error) {
				// Get filesystem.
				filesystem := vol.ConfigBlockFilesystem() // outer-vol.

				if vol.contentType == ContentTypeFS {
					_, err := makeFSType(rootBlockPath, filesystem, nil)
					if err != nil {
						return 0, err
					}
				}

				return 0, nil
			},
		}

		/*
			create volume will mount, create the image file, then call our filler, and unmount, and then we can take care of the mounting the side-car
			in MountVolume
		*/
		err := d.CreateVolume(fsImgVol, innerFiller, op)
		if err != nil {
			return err
		}
		revert.Add(func() { _ = d.DeleteVolume(fsImgVol, op) })
	}

	// for  block or fs-img we need to create a dataset
	if vol.contentType == ContentTypeBlock || isFsImgVol(vol) || (vol.contentType == ContentTypeFS && !needsFsImgVol(vol)) {

		/*
			for a VMBlock we need to create both a .block with an root.img and a filesystem
			volume. The filesystem volume has to be separate so that it can have a separate quota
			to the root.img/block volume.
		*/

		// Create the filesystem dataset.
		dataset := d.dataset(vol, false)

		err := d.createDataset(dataset) // TODO: we should set the filesystem on the dataset so that it can be recovered eventually in ListVolumes (and possibly mount options)
		if err != nil {
			return err
		}

		// now share it
		err = d.createNfsShare(dataset)
		if err != nil {
			return err
		}

		// Apply the size limit.
		// err = d.SetVolumeQuota(vol, vol.ConfigSize(), false, op)
		// if err != nil {
		// 	return err
		// }

		// Apply the blocksize.
		err = d.setBlocksizeFromConfig(vol)
		if err != nil {
			return err
		}
	}

	// For VM images, create a filesystem volume too.
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.CreateVolume(fsVol, nil, op)
		if err != nil {
			return err
		}

		revert.Add(func() { _ = d.DeleteVolume(fsVol, op) })
	}

	err = vol.MountTask(func(mountPath string, op *operations.Operation) error {

		// path to disk volume if volume is block or iso.
		var rootBlockPath string

		// If we are creating a block volume, resize it to the requested size or the default.
		// For block volumes, we expect the filler function to have converted the qcow2 image to raw into the rootBlockPath.
		// For ISOs the content will just be copied.
		if IsContentBlock(vol.contentType) || isFsImgVol(vol) {

			// TODO: this relies on "isBlockBacked" for fs-img blockbacked vols.
			// Convert to bytes.
			sizeBytes, err := units.ParseByteSizeString(vol.ConfigSize())
			if err != nil {
				return err
			}

			if sizeBytes == 0 {
				blockVol := vol.Clone()
				blockVol.contentType = ContentTypeBlock
				sizeBytes, err = units.ParseByteSizeString(blockVol.ConfigSize())
				if err != nil {
					return err
				}
			}

			// We expect the filler to copy the VM image into this path.
			rootBlockPath, err = d.GetVolumeDiskPath(vol)
			if err != nil {
				return err
			}

			// Ignore ErrCannotBeShrunk when setting size this just means the filler run above has needed to
			// increase the volume size beyond the default block volume size.
			_, err = ensureVolumeBlockFile(vol, rootBlockPath, sizeBytes, false)
			if err != nil && !errors.Is(err, ErrCannotBeShrunk) {
				return err
			}

			// Move the GPT alt header to end of disk if needed and if filler specified.
			if vol.IsVMBlock() && filler != nil && filler.Fill != nil {
				// err = d.moveGPTAltHeader(rootBlockPath)
				// if err != nil {
				// 	return err
				// }
			}
		}

		// Run the volume filler function if supplied.
		if filler != nil && filler.Fill != nil {
			var err error

			allowUnsafeResize := false
			if vol.volType == VolumeTypeImage {
				// Allow filler to resize initial image volume as needed.
				// Some storage drivers don't normally allow image volumes to be resized due to
				// them having read-only snapshots that cannot be resized. However when creating
				// the initial image volume and filling it before the snapshot is taken resizing
				// can be allowed and is required in order to support unpacking images larger than
				// the default volume size. The filler function is still expected to obey any
				// volume size restrictions configured on the pool.
				// Unsafe resize is also needed to disable filesystem resize safety checks.
				// This is safe because if for some reason an error occurs the volume will be
				// discarded rather than leaving a corrupt filesystem.
				allowUnsafeResize = true
			}

			// Run the filler.
			err = d.runFiller(vol, rootBlockPath, filler, allowUnsafeResize)
			if err != nil {
				return err
			}

			// Move the GPT alt header to end of disk if needed.
			if vol.IsVMBlock() { // TODO: this will corrupt our image that we lay down.
				// err = d.moveGPTAltHeader(rootBlockPath)
				// if err != nil {
				// 	return err
				// }
			}
		}

		//if vol.contentType == ContentTypeFS {
		// Run EnsureMountPath again after mounting and filling to ensure the mount directory has
		// the correct permissions set.
		err := vol.EnsureMountPath()
		if err != nil {
			return err
		}
		//}

		return nil
	}, op)
	if err != nil {
		return err
	}

	// Setup snapshot and unset mountpoint on image.
	if vol.volType == VolumeTypeImage && !isFsImgVol(vol) {

		// ideally, we don't want to snap the underlying when we create the img, but rather after we've unpacked.
		// note: we may need to sync the underlying filesystem, it depends if its still mounted, I think it shouldn't be.

		dataset := d.dataset(vol, false)
		snapName := fmt.Sprintf("%s@readonly", dataset)

		// Create snapshot of the main dataset.
		err := d.createSnapshot(snapName, false)
		if err != nil {
			return err
		}

	}

	// All done.
	revert.Success()

	return nil
}

// // CreateVolumeFromBackup re-creates a volume from its exported state.
// func (d *zfs) CreateVolumeFromBackup(vol Volume, srcBackup backup.Info, srcData io.ReadSeeker, op *operations.Operation) (VolumePostHook, revert.Hook, error) {
// 	// Handle the non-optimized tarballs through the generic unpacker.
// 	if !*srcBackup.OptimizedStorage {
// 		return genericVFSBackupUnpack(d, d.state.OS, vol, srcBackup.Snapshots, srcData, op)
// 	}

// 	volExists, err := d.HasVolume(vol)
// 	if err != nil {
// 		return nil, nil, err
// 	}

// 	if volExists {
// 		return nil, nil, fmt.Errorf("Cannot restore volume, already exists on target")
// 	}

// 	revert := revert.New()
// 	defer revert.Fail()

// 	// Define a revert function that will be used both to revert if an error occurs inside this
// 	// function but also return it for use from the calling functions if no error internally.
// 	revertHook := func() {
// 		for _, snapName := range srcBackup.Snapshots {
// 			fullSnapshotName := GetSnapshotVolumeName(vol.name, snapName)
// 			snapVol := NewVolume(d, d.name, vol.volType, vol.contentType, fullSnapshotName, vol.config, vol.poolConfig)
// 			_ = d.DeleteVolumeSnapshot(snapVol, op)
// 		}

// 		// And lastly the main volume.
// 		_ = d.DeleteVolume(vol, op)
// 	}

// 	// Only execute the revert function if we have had an error internally.
// 	revert.Add(revertHook)

// 	// Define function to unpack a volume from a backup tarball file.
// 	unpackVolume := func(v Volume, r io.ReadSeeker, unpacker []string, srcFile string, target string) error {
// 		d.Logger().Debug("Unpacking optimized volume", logger.Ctx{"source": srcFile, "target": target})

// 		targetPath := fmt.Sprintf("%s/storage-pools/%s", internalUtil.VarPath(""), target)
// 		tr, cancelFunc, err := archive.CompressedTarReader(context.Background(), r, unpacker, targetPath)
// 		if err != nil {
// 			return err
// 		}

// 		defer cancelFunc()

// 		for {
// 			hdr, err := tr.Next()
// 			if err == io.EOF {
// 				break // End of archive.
// 			}

// 			if err != nil {
// 				return err
// 			}

// 			if hdr.Name == srcFile {
// 				// Extract the backup.
// 				if v.ContentType() == ContentTypeBlock || d.isBlockBacked(v) {
// 					err = subprocess.RunCommandWithFds(context.TODO(), tr, nil, "zfs", "receive", "-F", target)
// 				} else {
// 					err = subprocess.RunCommandWithFds(context.TODO(), tr, nil, "zfs", "receive", "-x", "mountpoint", "-F", target)
// 				}

// 				if err != nil {
// 					return err
// 				}

// 				cancelFunc()
// 				return nil
// 			}
// 		}

// 		return fmt.Errorf("Could not find %q", srcFile)
// 	}

// 	var postHook VolumePostHook

// 	// Create a list of actual volumes to unpack.
// 	var vols []Volume
// 	if vol.IsVMBlock() {
// 		vols = append(vols, vol.NewVMBlockFilesystemVolume())
// 	}

// 	vols = append(vols, vol)

// 	for _, v := range vols {
// 		// Find the compression algorithm used for backup source data.
// 		_, err := srcData.Seek(0, io.SeekStart)
// 		if err != nil {
// 			return nil, nil, err
// 		}

// 		_, _, unpacker, err := archive.DetectCompressionFile(srcData)
// 		if err != nil {
// 			return nil, nil, err
// 		}

// 		if len(srcBackup.Snapshots) > 0 {
// 			// Create new snapshots directory.
// 			err := createParentSnapshotDirIfMissing(d.name, v.volType, v.name)
// 			if err != nil {
// 				return nil, nil, err
// 			}
// 		}

// 		// Restore backups from oldest to newest.
// 		for _, snapName := range srcBackup.Snapshots {
// 			prefix := "snapshots"
// 			fileName := fmt.Sprintf("%s.bin", snapName)
// 			if v.volType == VolumeTypeVM {
// 				prefix = "virtual-machine-snapshots"
// 				if v.contentType == ContentTypeFS {
// 					fileName = fmt.Sprintf("%s-config.bin", snapName)
// 				}
// 			} else if v.volType == VolumeTypeCustom {
// 				prefix = "volume-snapshots"
// 			}

// 			srcFile := fmt.Sprintf("backup/%s/%s", prefix, fileName)
// 			dstSnapshot := fmt.Sprintf("%s@snapshot-%s", d.dataset(v, false), snapName)
// 			err = unpackVolume(v, srcData, unpacker, srcFile, dstSnapshot)
// 			if err != nil {
// 				return nil, nil, err
// 			}
// 		}

// 		// Extract main volume.
// 		fileName := "container.bin"
// 		if v.volType == VolumeTypeVM {
// 			if v.contentType == ContentTypeFS {
// 				fileName = "virtual-machine-config.bin"
// 			} else {
// 				fileName = "virtual-machine.bin"
// 			}
// 		} else if v.volType == VolumeTypeCustom {
// 			fileName = "volume.bin"
// 		}

// 		err = unpackVolume(v, srcData, unpacker, fmt.Sprintf("backup/%s", fileName), d.dataset(v, false))
// 		if err != nil {
// 			return nil, nil, err
// 		}

// 		// Strip internal snapshots.
// 		entries, err := d.getDatasets(d.dataset(v, false), "snapshot")
// 		if err != nil {
// 			return nil, nil, err
// 		}

// 		// Remove only the internal snapshots.
// 		for _, entry := range entries {
// 			if strings.Contains(entry, "@snapshot-") {
// 				continue
// 			}

// 			if strings.Contains(entry, "@") {
// 				_, err := subprocess.RunCommand("zfs", "destroy", fmt.Sprintf("%s%s", d.dataset(v, false), entry))
// 				if err != nil {
// 					return nil, nil, err
// 				}
// 			}
// 		}

// 		// Re-apply the base mount options.
// 		if v.contentType == ContentTypeFS {
// 			if zfsDelegate {
// 				// Unset the zoned property so the mountpoint property can be updated.
// 				err := d.setDatasetProperties(d.dataset(v, false), "zoned=off")
// 				if err != nil {
// 					return nil, nil, err
// 				}
// 			}

// 			err := d.setDatasetProperties(d.dataset(v, false), "mountpoint=legacy", "canmount=noauto")
// 			if err != nil {
// 				return nil, nil, err
// 			}

// 			// Apply the blocksize.
// 			err = d.setBlocksizeFromConfig(v)
// 			if err != nil {
// 				return nil, nil, err
// 			}
// 		}

// 		// Only mount instance filesystem volumes for backup.yaml access.
// 		if v.volType != VolumeTypeCustom && v.contentType != ContentTypeBlock {
// 			// The import requires a mounted volume, so mount it and have it unmounted as a post hook.
// 			err = d.MountVolume(v, op)
// 			if err != nil {
// 				return nil, nil, err
// 			}

// 			revert.Add(func() { _, _ = d.UnmountVolume(v, false, op) })

// 			postHook = func(postVol Volume) error {
// 				_, err := d.UnmountVolume(postVol, false, op)
// 				return err
// 			}
// 		}
// 	}

// 	cleanup := revert.Clone().Fail // Clone before calling revert.Success() so we can return the Fail func.
// 	revert.Success()
// 	return postHook, cleanup, nil
// }

// CreateVolumeFromCopy provides same-pool volume copying functionality.
func (d *truenas) CreateVolumeFromCopy(vol Volume, srcVol Volume, copySnapshots bool, allowInconsistent bool, op *operations.Operation) error {
	var err error

	// Revert handling
	revert := revert.New()
	defer revert.Fail()

	//if vol.contentType == ContentTypeFS {
	// Create mountpoint.
	err = vol.EnsureMountPath()
	if err != nil {
		return err
	}

	revert.Add(func() { _ = os.Remove(vol.MountPath()) })
	//}

	// // For VMs, also copy the filesystem dataset.
	// if vol.IsVMBlock() {
	// 	// For VMs, also copy the filesystem volume.
	// 	srcFSVol := srcVol.NewVMBlockFilesystemVolume()
	// 	fsVol := vol.NewVMBlockFilesystemVolume()

	// 	err = d.CreateVolumeFromCopy(fsVol, srcFSVol, copySnapshots, false, op)
	// 	if err != nil {
	// 		return err
	// 	}

	// 	// Delete on revert.
	// 	revert.Add(func() { _ = d.DeleteVolume(fsVol, op) })
	// }

	// Retrieve snapshots on the source.
	snapshots := []string{}
	if !srcVol.IsSnapshot() && copySnapshots {
		snapshots, err = d.VolumeSnapshots(srcVol, op)
		if err != nil {
			return err
		}
	}

	skipNfsShare := false

	// When not allowing inconsistent copies and the volume has a mounted filesystem, we must ensure it is
	// consistent by syncing. Ideally we'd freeze the fs too.
	sourcePath := srcVol.MountPath()
	if !allowInconsistent && linux.IsMountPoint(sourcePath) {
		/*
			The Instance was already frozen if it were running. Incus tries to flush the filesystem, but
			it only flushes lxc rootfs directories. We need to separately flush the whole NFS mount which
			contains the root.img if applicable.
		*/
		err := linux.SyncFS(sourcePath)
		if err != nil {
			return fmt.Errorf("Failed syncing filesystem %q: %w", sourcePath, err)
		}
		/*
			if we have the guest frozen, we want to skip anything which will delay unfreezing
		*/
		//skipNfsShare = true
	}

	var srcSnapshot string
	if srcVol.volType == VolumeTypeImage {
		srcSnapshot = fmt.Sprintf("%s@readonly", d.dataset(srcVol, false))
	} else if srcVol.IsSnapshot() {
		srcSnapshot = d.dataset(srcVol, false)
	} else {
		// Create a new snapshot for copy.
		srcSnapshot = fmt.Sprintf("%s@copy-%s", d.dataset(srcVol, false), uuid.New().String())

		err := d.createSnapshot(srcSnapshot, false)
		if err != nil {
			return err
		}

		// If truenas.clone_copy is disabled delete the snapshot at the end.
		if util.IsFalse(d.config["truenas.clone_copy"]) || len(snapshots) > 0 {
			// Delete the snapshot at the end.
			defer func() {
				// Delete snapshot (or mark for deferred deletion if cannot be deleted currently).
				//_, err := subprocess.RunCommand("zfs", "destroy", "-r", "-d", srcSnapshot)
				out, err := d.runTool("snapshot", "delete", "-r", "--defer", srcSnapshot)
				_ = out

				if err != nil {
					d.logger.Warn("Failed deleting temporary snapshot for copy", logger.Ctx{"snapshot": srcSnapshot, "err": err})
				}
			}()
		} else {
			// Delete the snapshot on revert.
			revert.Add(func() {
				// Delete snapshot (or mark for deferred deletion if cannot be deleted currently).
				//_, err := subprocess.RunCommand("zfs", "destroy", "-r", "-d", srcSnapshot)
				out, err := d.runTool("snapshot", "delete", "-r", "--defer", srcSnapshot)
				_ = out
				if err != nil {
					d.logger.Warn("Failed deleting temporary snapshot for copy", logger.Ctx{"snapshot": srcSnapshot, "err": err})
				}
			})
		}
	}

	// Delete the volume created on failure.
	revert.Add(func() { _ = d.DeleteVolume(vol, op) })

	// If truenas.clone_copy is disabled or source volume has snapshots, then use full copy mode.
	if util.IsFalse(d.config["truenas.clone_copy"]) || len(snapshots) > 0 {
		snapName := strings.SplitN(srcSnapshot, "@", 2)[1]

		// NOTE: we have not implemented "zfs send/recieve" yet. WIll be performed using replication.run_onetime task
		if true {
			flag := "instance-only"
			if srcVol.volType == VolumeTypeCustom {
				flag = "volume-only"
			}
			return fmt.Errorf("Failed to copy volume with snapshots (not implemented). Try `--%s` to skip the snapshots", flag)
		}

		// Send/receive the snapshot.
		var sender *exec.Cmd
		var receiver *exec.Cmd
		if vol.ContentType() == ContentTypeBlock || d.isBlockBacked(vol) {
			receiver = exec.Command("zfs", "receive", d.dataset(vol, false))
		} else {
			receiver = exec.Command("zfs", "receive", "-x", "mountpoint", d.dataset(vol, false))
		}

		// Handle transferring snapshots.
		if len(snapshots) > 0 {
			args := []string{"send", "-R"}

			// Use raw flag is supported, this is required to send/receive encrypted volumes (and enables compression).
			if zfsRaw {
				args = append(args, "-w")
			}

			args = append(args, srcSnapshot)

			sender = exec.Command("zfs", args...)
		} else {
			args := []string{"send"}

			// Check if nesting is required.
			if d.needsRecursion(d.dataset(srcVol, false)) {
				args = append(args, "-R")

				if zfsRaw {
					args = append(args, "-w")
				}
			}

			if d.config["truenas.clone_copy"] == "rebase" {
				var err error
				origin := d.dataset(srcVol, false)
				for {
					fields := strings.SplitN(origin, "@", 2)

					// If the origin is a @readonly snapshot under a /images/ path (/images or deleted/images), we're done.
					if len(fields) > 1 && strings.Contains(fields[0], "/images/") && fields[1] == "readonly" {
						break
					}

					origin, err = d.getDatasetProperty(origin, "origin")
					if err != nil {
						return err
					}

					if origin == "" || origin == "-" {
						origin = ""
						break
					}
				}

				if origin != "" && origin != srcSnapshot {
					args = append(args, "-i", origin)
					args = append(args, srcSnapshot)
					sender = exec.Command("zfs", args...)
				} else {
					args = append(args, srcSnapshot)
					sender = exec.Command("zfs", args...)
				}
			} else {
				args = append(args, srcSnapshot)
				sender = exec.Command("zfs", args...)
			}
		}

		// Configure the pipes.
		receiver.Stdin, _ = sender.StdoutPipe()
		receiver.Stdout = os.Stdout

		var recvStderr bytes.Buffer
		receiver.Stderr = &recvStderr

		var sendStderr bytes.Buffer
		sender.Stderr = &sendStderr

		// Run the transfer.
		err := receiver.Start()
		if err != nil {
			return fmt.Errorf("Failed starting ZFS receive: %w", err)
		}

		err = sender.Start()
		if err != nil {
			_ = receiver.Process.Kill()
			return fmt.Errorf("Failed starting ZFS send: %w", err)
		}

		senderErr := make(chan error)
		go func() {
			err := sender.Wait()
			if err != nil {
				_ = receiver.Process.Kill()

				// This removes any newlines in the error message.
				msg := strings.ReplaceAll(strings.TrimSpace(sendStderr.String()), "\n", " ")

				senderErr <- fmt.Errorf("Failed ZFS send: %w (%s)", err, msg)
				return
			}

			senderErr <- nil
		}()

		err = receiver.Wait()
		if err != nil {
			_ = sender.Process.Kill()

			// This removes any newlines in the error message.
			msg := strings.ReplaceAll(strings.TrimSpace(recvStderr.String()), "\n", " ")

			return fmt.Errorf("Failed ZFS receive: %w (%s)", err, msg)
		}

		err = <-senderErr
		if err != nil {
			return err
		}

		// Delete the snapshot.
		//_, err = subprocess.RunCommand("zfs", "destroy", "-r", fmt.Sprintf("%s@%s", d.dataset(vol, false), snapName))
		_, err = d.runTool("snapshot", "delete", "-r", fmt.Sprintf("%s@%s", d.dataset(vol, false), snapName))
		if err != nil {
			return err
		}

		// Cleanup unexpected snapshots.
		if len(snapshots) > 0 {
			children, err := d.getDatasets(d.dataset(vol, false), "snapshot")
			if err != nil {
				return err
			}

			toDestroy := make([]string, 0)
			for _, entry := range children {
				// Check if expected snapshot.
				if strings.Contains(entry, "@snapshot-") {
					name := strings.Split(entry, "@snapshot-")[1]
					if slices.Contains(snapshots, name) {
						continue
					}
				}

				// Delete the rest.
				toDestroy = append(toDestroy, fmt.Sprintf("%s%s", d.dataset(vol, false), entry))
			}
			if len(toDestroy) > 0 {
				snapDelCmd := []string{"snapshot", "delete"}
				_, err := d.runTool(append(snapDelCmd, toDestroy...)...)
				if err != nil {
					return err
				}
			}
		}
	} else {
		// Perform volume clone.
		args := []string{"snapshot", "clone"}
		dataset := d.dataset(vol, false)
		args = append(args, srcSnapshot, dataset)
		// Clone the snapshot.
		out, err := d.runTool(args...)
		_ = out

		if err != nil {
			return err
		}

		// and share the clone.
		if !skipNfsShare {
			/*
				this can take a while, and we have a fallback in Mount if it hasn't been done, so
				when we have the guest frozen, we may skip it.
			*/
			err = d.createNfsShare(dataset)
			if err != nil {
				return err
			}
		}

	}

	// Apply the properties.
	if vol.contentType == ContentTypeFS {
		if !d.isBlockBacked(srcVol) {
			// err := d.setDatasetProperties(d.dataset(vol, false), "mountpoint=legacy", "canmount=noauto")
			// if err != nil {
			// 	return err
			// }

			// Apply the blocksize.
			err = d.setBlocksizeFromConfig(vol)
			if err != nil {
				return err
			}
		}

		// would be better to mount where below does "activate"

		// Mounts the volume and ensure the permissions are set correctly inside the mounted volume.
		verifyVolMountPath := func() error {
			return vol.MountTask(func(_ string, _ *operations.Operation) error {
				return vol.EnsureMountPath()
			}, op)
		}

		if d.isBlockBacked(srcVol) && renegerateFilesystemUUIDNeeded(vol.ConfigBlockFilesystem()) {

			// regen must be done with vol unmounted.

			// _, err := d.activateVolume(vol)
			// if err != nil {
			// 	return err
			// }

			// TODO: to do this we need to mount the fs-img.
			fsImgVol := cloneVolAsFsImgVol(vol)
			err := fsImgVol.MountTask(func(mountPath string, op *operations.Operation) error {

				rootBlockPath, err := d.GetVolumeDiskPath(fsImgVol)
				if err != nil {
					return err
				}

				d.logger.Debug("Regenerating filesystem UUID", logger.Ctx{"dev": rootBlockPath, "fs": vol.ConfigBlockFilesystem()})
				err = regenerateFilesystemUUID(vol.ConfigBlockFilesystem(), rootBlockPath)
				if err != nil {
					return err
				}

				// performing the mount while the fsImg is mounted prevents repeated remounts.
				err = verifyVolMountPath()
				if err != nil {
					return err
				}

				return nil
			}, op)

			if err != nil {
				return err
			}

		} else {
			// Mount the volume and ensure the permissions are set correctly inside the mounted volume.
			err = verifyVolMountPath()
			if err != nil {
				return err
			}
		}
	}

	// Pass allowUnsafeResize as true when resizing block backed filesystem volumes because we want to allow
	// the filesystem to be shrunk as small as possible without needing the safety checks that would prevent
	// leaving the filesystem in an inconsistent state if the resize couldn't be completed. This is because if
	// the resize fails we will delete the volume anyway so don't have to worry about it being inconsistent.
	var allowUnsafeResize bool
	_ = allowUnsafeResize

	if d.isBlockBacked(vol) && vol.contentType == ContentTypeFS {
		allowUnsafeResize = true
	}

	// Resize volume to the size specified. Only uses volume "size" property and does not use pool/defaults
	// to give the caller more control over the size being used.
	// err = d.SetVolumeQuota(vol, vol.config["size"], allowUnsafeResize, op)
	// if err != nil {
	// 	return err
	// }

	// All done.
	revert.Success()
	return nil
}

// // CreateVolumeFromMigration creates a volume being sent via a migration.
// func (d *zfs) CreateVolumeFromMigration(vol Volume, conn io.ReadWriteCloser, volTargetArgs localMigration.VolumeTargetArgs, preFiller *VolumeFiller, op *operations.Operation) error {
// 	// Handle simple rsync and block_and_rsync through generic.
// 	if volTargetArgs.MigrationType.FSType == migration.MigrationFSType_RSYNC || volTargetArgs.MigrationType.FSType == migration.MigrationFSType_BLOCK_AND_RSYNC {
// 		return genericVFSCreateVolumeFromMigration(d, nil, vol, conn, volTargetArgs, preFiller, op)
// 	} else if volTargetArgs.MigrationType.FSType != migration.MigrationFSType_ZFS {
// 		return ErrNotSupported
// 	}

// 	var migrationHeader ZFSMetaDataHeader

// 	// If no snapshots have been provided it can mean two things:
// 	// 1) The target has no snapshots
// 	// 2) Snapshots shouldn't be copied (--instance-only flag)
// 	volumeOnly := len(volTargetArgs.Snapshots) == 0

// 	if slices.Contains(volTargetArgs.MigrationType.Features, migration.ZFSFeatureMigrationHeader) {
// 		// The source will send all of its snapshots with their respective GUID.
// 		buf, err := io.ReadAll(conn)
// 		if err != nil {
// 			return fmt.Errorf("Failed reading ZFS migration header: %w", err)
// 		}

// 		err = json.Unmarshal(buf, &migrationHeader)
// 		if err != nil {
// 			return fmt.Errorf("Failed decoding ZFS migration header: %w", err)
// 		}
// 	}

// 	// If we're refreshing, send back all snapshots of the target.
// 	if volTargetArgs.Refresh && slices.Contains(volTargetArgs.MigrationType.Features, migration.ZFSFeatureMigrationHeader) {
// 		snapshots, err := vol.Snapshots(op)
// 		if err != nil {
// 			return fmt.Errorf("Failed getting volume snapshots: %w", err)
// 		}

// 		// If there are no snapshots on the target, there's no point in doing an optimized
// 		// refresh.
// 		if len(snapshots) == 0 {
// 			volTargetArgs.Refresh = false
// 		}

// 		var respSnapshots []ZFSDataset
// 		var syncSnapshotNames []string

// 		// Get the GUIDs of all target snapshots.
// 		for _, snapVol := range snapshots {
// 			guid, err := d.getDatasetProperty(d.dataset(snapVol, false), "guid")
// 			if err != nil {
// 				return err
// 			}

// 			_, snapName, _ := api.GetParentAndSnapshotName(snapVol.name)

// 			respSnapshots = append(respSnapshots, ZFSDataset{Name: snapName, GUID: guid})
// 		}

// 		// Generate list of snapshots which need to be synced, i.e. are available on the source but not on the target.
// 		for _, srcSnapshot := range migrationHeader.SnapshotDatasets {
// 			found := false

// 			for _, dstSnapshot := range respSnapshots {
// 				if srcSnapshot.GUID == dstSnapshot.GUID {
// 					found = true
// 					break
// 				}
// 			}

// 			if !found {
// 				syncSnapshotNames = append(syncSnapshotNames, srcSnapshot.Name)
// 			}
// 		}

// 		// The following scenario will result in a failure:
// 		// - The source has more than one snapshot
// 		// - The target has at least one of these snapshot, but not the very first
// 		//
// 		// It will fail because the source tries sending the first snapshot using `zfs send <first>`.
// 		// Since the target does have snapshots, `zfs receive` will fail with:
// 		//     cannot receive new filesystem stream: destination has snapshots
// 		//
// 		// We therefore need to check the snapshots, and delete all target snapshots if the above
// 		// scenario is true.
// 		if !volumeOnly && len(respSnapshots) > 0 && len(migrationHeader.SnapshotDatasets) > 0 && respSnapshots[0].GUID != migrationHeader.SnapshotDatasets[0].GUID {
// 			for _, snapVol := range snapshots {
// 				// Delete
// 				err = d.DeleteVolume(snapVol, op)
// 				if err != nil {
// 					return err
// 				}
// 			}

// 			// Let the source know that we don't have any snapshots.
// 			respSnapshots = []ZFSDataset{}

// 			// Let the source know that we need all snapshots.
// 			syncSnapshotNames = []string{}

// 			for _, dataset := range migrationHeader.SnapshotDatasets {
// 				syncSnapshotNames = append(syncSnapshotNames, dataset.Name)
// 			}
// 		} else {
// 			// Delete local snapshots which exist on the target but not on the source.
// 			for _, snapVol := range snapshots {
// 				targetOnlySnapshot := true
// 				_, snapName, _ := api.GetParentAndSnapshotName(snapVol.name)

// 				for _, migrationSnap := range migrationHeader.SnapshotDatasets {
// 					if snapName == migrationSnap.Name {
// 						targetOnlySnapshot = false
// 						break
// 					}
// 				}

// 				if targetOnlySnapshot {
// 					// Delete
// 					err = d.DeleteVolume(snapVol, op)
// 					if err != nil {
// 						return err
// 					}
// 				}
// 			}
// 		}

// 		migrationHeader = ZFSMetaDataHeader{}
// 		migrationHeader.SnapshotDatasets = respSnapshots

// 		// Send back all target snapshots with their GUIDs.
// 		headerJSON, err := json.Marshal(migrationHeader)
// 		if err != nil {
// 			return fmt.Errorf("Failed encoding ZFS migration header: %w", err)
// 		}

// 		_, err = conn.Write(headerJSON)
// 		if err != nil {
// 			return fmt.Errorf("Failed sending ZFS migration header: %w", err)
// 		}

// 		err = conn.Close() //End the frame.
// 		if err != nil {
// 			return fmt.Errorf("Failed closing ZFS migration header frame: %w", err)
// 		}

// 		// Don't pass the snapshots if it's volume only.
// 		if !volumeOnly {
// 			volTargetArgs.Snapshots = syncSnapshotNames
// 		}
// 	}

// 	return d.createVolumeFromMigrationOptimized(vol, conn, volTargetArgs, volumeOnly, preFiller, op)
// }

// func (d *zfs) createVolumeFromMigrationOptimized(vol Volume, conn io.ReadWriteCloser, volTargetArgs localMigration.VolumeTargetArgs, volumeOnly bool, preFiller *VolumeFiller, op *operations.Operation) error {
// 	if vol.IsVMBlock() {
// 		fsVol := vol.NewVMBlockFilesystemVolume()
// 		err := d.createVolumeFromMigrationOptimized(fsVol, conn, volTargetArgs, volumeOnly, preFiller, op)
// 		if err != nil {
// 			return err
// 		}
// 	}

// 	var snapshots []Volume
// 	var err error

// 	// Rollback to the latest identical snapshot if performing a refresh.
// 	if volTargetArgs.Refresh {
// 		snapshots, err = vol.Snapshots(op)
// 		if err != nil {
// 			return err
// 		}

// 		if len(snapshots) > 0 {
// 			lastIdenticalSnapshot := snapshots[len(snapshots)-1]
// 			_, lastIdenticalSnapshotOnlyName, _ := api.GetParentAndSnapshotName(lastIdenticalSnapshot.Name())

// 			err = d.restoreVolume(vol, lastIdenticalSnapshotOnlyName, true, op)
// 			if err != nil {
// 				return err
// 			}
// 		}
// 	}

// 	revert := revert.New()
// 	defer revert.Fail()

// 	// Handle zfs send/receive migration.
// 	if len(volTargetArgs.Snapshots) > 0 {
// 		// Create the parent directory.
// 		err := createParentSnapshotDirIfMissing(d.name, vol.volType, vol.name)
// 		if err != nil {
// 			return err
// 		}

// 		// Transfer the snapshots.
// 		for _, snapName := range volTargetArgs.Snapshots {
// 			snapVol, err := vol.NewSnapshot(snapName)
// 			if err != nil {
// 				return err
// 			}

// 			// Setup progress tracking.
// 			var wrapper *ioprogress.ProgressTracker
// 			if volTargetArgs.TrackProgress {
// 				wrapper = localMigration.ProgressTracker(op, "fs_progress", snapVol.Name())
// 			}

// 			err = d.receiveDataset(snapVol, conn, wrapper)
// 			if err != nil {
// 				_ = d.DeleteVolume(snapVol, op)
// 				return fmt.Errorf("Failed receiving snapshot volume %q: %w", snapVol.Name(), err)
// 			}

// 			revert.Add(func() {
// 				_ = d.DeleteVolumeSnapshot(snapVol, op)
// 			})
// 		}
// 	}

// 	if !volTargetArgs.Refresh {
// 		revert.Add(func() {
// 			_ = d.DeleteVolume(vol, op)
// 		})
// 	}

// 	// Setup progress tracking.
// 	var wrapper *ioprogress.ProgressTracker
// 	if volTargetArgs.TrackProgress {
// 		wrapper = localMigration.ProgressTracker(op, "fs_progress", vol.name)
// 	}

// 	// Transfer the main volume.
// 	err = d.receiveDataset(vol, conn, wrapper)
// 	if err != nil {
// 		return fmt.Errorf("Failed receiving volume %q: %w", vol.Name(), err)
// 	}

// 	// Strip internal snapshots.
// 	entries, err := d.getDatasets(d.dataset(vol, false), "snapshot")
// 	if err != nil {
// 		return err
// 	}

// 	// keepDataset returns whether to keep the data set or delete it. Data sets that are non-snapshots or
// 	// snapshots that match the requested snapshots in volTargetArgs.Snapshots are kept. Any other snapshot
// 	// data sets should be removed.
// 	keepDataset := func(dataSetName string) bool {
// 		// Keep non-snapshot data sets and snapshots that don't have the snapshot prefix indicator.
// 		dataSetSnapshotPrefix := "@snapshot-"
// 		if !strings.HasPrefix(dataSetName, "@") || !strings.HasPrefix(dataSetName, dataSetSnapshotPrefix) {
// 			return false
// 		}

// 		// Check if snapshot data set matches one of the requested snapshots in volTargetArgs.Snapshots.
// 		// If so, then keep it, otherwise request it be removed.
// 		entrySnapName := strings.TrimPrefix(dataSetName, dataSetSnapshotPrefix)
// 		for _, snapName := range volTargetArgs.Snapshots {
// 			if entrySnapName == snapName {
// 				return true // Keep snapshot data set if present in the requested snapshots list.
// 			}
// 		}

// 		return false // Delete any other snapshot data sets that have been transferred.
// 	}

// 	if volTargetArgs.Refresh {
// 		// Only delete the latest migration snapshot.
// 		_, err := subprocess.RunCommand("zfs", "destroy", "-r", fmt.Sprintf("%s%s", d.dataset(vol, false), entries[len(entries)-1]))
// 		if err != nil {
// 			return err
// 		}
// 	} else {
// 		// Remove any snapshots that were transferred but are not needed.
// 		for _, entry := range entries {
// 			if !keepDataset(entry) {
// 				_, err := subprocess.RunCommand("zfs", "destroy", fmt.Sprintf("%s%s", d.dataset(vol, false), entry))
// 				if err != nil {
// 					return err
// 				}
// 			}
// 		}
// 	}

// 	if vol.contentType == ContentTypeFS {
// 		// Create mountpoint.
// 		err := vol.EnsureMountPath()
// 		if err != nil {
// 			return err
// 		}

// 		if !d.isBlockBacked(vol) {
// 			// Re-apply the base mount options.
// 			if zfsDelegate {
// 				// Unset the zoned property so the mountpoint property can be updated.
// 				err := d.setDatasetProperties(d.dataset(vol, false), "zoned=off")
// 				if err != nil {
// 					return err
// 				}
// 			}

// 			err = d.setDatasetProperties(d.dataset(vol, false), "mountpoint=legacy", "canmount=noauto")
// 			if err != nil {
// 				return err
// 			}

// 			// Apply the size limit.
// 			err = d.SetVolumeQuota(vol, vol.ConfigSize(), false, op)
// 			if err != nil {
// 				return err
// 			}

// 			// Apply the blocksize.
// 			err = d.setBlocksizeFromConfig(vol)
// 			if err != nil {
// 				return err
// 			}
// 		}

// 		if d.isBlockBacked(vol) && renegerateFilesystemUUIDNeeded(vol.ConfigBlockFilesystem()) {
// 			// Activate volume if needed.
// 			activated, err := d.activateVolume(vol)
// 			if err != nil {
// 				return err
// 			}

// 			if activated {
// 				defer func() { _, _ = d.deactivateVolume(vol) }()
// 			}

// 			volPath, err := d.GetVolumeDiskPath(vol)
// 			if err != nil {
// 				return err
// 			}

// 			d.logger.Debug("Regenerating filesystem UUID", logger.Ctx{"dev": volPath, "fs": vol.ConfigBlockFilesystem()})
// 			err = regenerateFilesystemUUID(vol.ConfigBlockFilesystem(), volPath)
// 			if err != nil {
// 				return err
// 			}
// 		}
// 	}

// 	revert.Success()
// 	return nil
// }

// // RefreshVolume updates an existing volume to match the state of another.
// func (d *zfs) RefreshVolume(vol Volume, srcVol Volume, srcSnapshots []Volume, allowInconsistent bool, op *operations.Operation) error {
// 	var err error
// 	var targetSnapshots []Volume
// 	var srcSnapshotsAll []Volume

// 	if !srcVol.IsSnapshot() {
// 		// Get target snapshots
// 		targetSnapshots, err = vol.Snapshots(op)
// 		if err != nil {
// 			return fmt.Errorf("Failed to get target snapshots: %w", err)
// 		}

// 		srcSnapshotsAll, err = srcVol.Snapshots(op)
// 		if err != nil {
// 			return fmt.Errorf("Failed to get source snapshots: %w", err)
// 		}
// 	}

// 	// If there are no target or source snapshots, perform a simple copy using zfs.
// 	// We cannot use generic vfs volume copy here, as zfs will complain if a generic
// 	// copy/refresh is followed by an optimized refresh.
// 	if len(targetSnapshots) == 0 || len(srcSnapshotsAll) == 0 {
// 		err = d.DeleteVolume(vol, op)
// 		if err != nil {
// 			return err
// 		}

// 		return d.CreateVolumeFromCopy(vol, srcVol, len(srcSnapshots) > 0, false, op)
// 	}

// 	transfer := func(src Volume, target Volume, origin Volume) error {
// 		var sender *exec.Cmd

// 		receiver := exec.Command("zfs", "receive", d.dataset(target, false))

// 		args := []string{"send"}

// 		// Check if nesting is required.
// 		if d.needsRecursion(d.dataset(src, false)) {
// 			args = append(args, "-R")

// 			if zfsRaw {
// 				args = append(args, "-w")
// 			}
// 		}

// 		if origin.Name() != src.Name() {
// 			args = append(args, "-i", d.dataset(origin, false), d.dataset(src, false))
// 			sender = exec.Command("zfs", args...)
// 		} else {
// 			args = append(args, d.dataset(src, false))
// 			sender = exec.Command("zfs", args...)
// 		}

// 		// Configure the pipes.
// 		receiver.Stdin, _ = sender.StdoutPipe()
// 		receiver.Stdout = os.Stdout

// 		var recvStderr bytes.Buffer
// 		receiver.Stderr = &recvStderr

// 		var sendStderr bytes.Buffer
// 		sender.Stderr = &sendStderr

// 		// Run the transfer.
// 		err := receiver.Start()
// 		if err != nil {
// 			return fmt.Errorf("Failed starting ZFS receive: %w", err)
// 		}

// 		err = sender.Start()
// 		if err != nil {
// 			_ = receiver.Process.Kill()
// 			return fmt.Errorf("Failed starting ZFS send: %w", err)
// 		}

// 		senderErr := make(chan error)
// 		go func() {
// 			err := sender.Wait()
// 			if err != nil {
// 				_ = receiver.Process.Kill()

// 				// This removes any newlines in the error message.
// 				msg := strings.ReplaceAll(strings.TrimSpace(sendStderr.String()), "\n", " ")

// 				senderErr <- fmt.Errorf("Failed ZFS send: %w (%s)", err, msg)
// 				return
// 			}

// 			senderErr <- nil
// 		}()

// 		err = receiver.Wait()
// 		if err != nil {
// 			_ = sender.Process.Kill()

// 			// This removes any newlines in the error message.
// 			msg := strings.ReplaceAll(strings.TrimSpace(recvStderr.String()), "\n", " ")

// 			if strings.Contains(msg, "does not match incremental source") {
// 				return ErrSnapshotDoesNotMatchIncrementalSource
// 			}

// 			return fmt.Errorf("Failed ZFS receive: %w (%s)", err, msg)
// 		}

// 		err = <-senderErr
// 		if err != nil {
// 			return err
// 		}

// 		return nil
// 	}

// 	// This represents the most recent identical snapshot of the source volume and target volume.
// 	lastIdenticalSnapshot := targetSnapshots[len(targetSnapshots)-1]
// 	_, lastIdenticalSnapshotOnlyName, _ := api.GetParentAndSnapshotName(lastIdenticalSnapshot.Name())

// 	// Rollback target volume to the latest identical snapshot
// 	err = d.RestoreVolume(vol, lastIdenticalSnapshotOnlyName, op)
// 	if err != nil {
// 		return fmt.Errorf("Failed to restore volume: %w", err)
// 	}

// 	// Create all missing snapshots on the target using an incremental stream
// 	for i, snap := range srcSnapshots {
// 		var originSnap Volume

// 		if i == 0 {
// 			originSnap, err = srcVol.NewSnapshot(lastIdenticalSnapshotOnlyName)
// 			if err != nil {
// 				return fmt.Errorf("Failed to create new snapshot volume: %w", err)
// 			}
// 		} else {
// 			originSnap = srcSnapshots[i-1]
// 		}

// 		err = transfer(snap, vol, originSnap)
// 		if err != nil {
// 			// Don't fail here. If it's not possible to perform an optimized refresh, do a generic
// 			// refresh instead.
// 			if errors.Is(err, ErrSnapshotDoesNotMatchIncrementalSource) {
// 				d.logger.Debug("Unable to perform an optimized refresh, doing a generic refresh", logger.Ctx{"err": err})
// 				return genericVFSCopyVolume(d, nil, vol, srcVol, srcSnapshots, true, allowInconsistent, op)
// 			}

// 			return fmt.Errorf("Failed to transfer snapshot %q: %w", snap.name, err)
// 		}

// 		if snap.IsVMBlock() {
// 			srcFSVol := snap.NewVMBlockFilesystemVolume()
// 			targetFSVol := vol.NewVMBlockFilesystemVolume()
// 			originFSVol := originSnap.NewVMBlockFilesystemVolume()

// 			err = transfer(srcFSVol, targetFSVol, originFSVol)
// 			if err != nil {
// 				// Don't fail here. If it's not possible to perform an optimized refresh, do a generic
// 				// refresh instead.
// 				if errors.Is(err, ErrSnapshotDoesNotMatchIncrementalSource) {
// 					d.logger.Debug("Unable to perform an optimized refresh, doing a generic refresh", logger.Ctx{"err": err})
// 					return genericVFSCopyVolume(d, nil, vol, srcVol, srcSnapshots, true, allowInconsistent, op)
// 				}

// 				return fmt.Errorf("Failed to transfer snapshot %q: %w", snap.name, err)
// 			}
// 		}
// 	}

// 	// Create temporary snapshot of the source volume.
// 	snapUUID := uuid.New().String()

// 	srcSnap, err := srcVol.NewSnapshot(snapUUID)
// 	if err != nil {
// 		return err
// 	}

// 	err = d.CreateVolumeSnapshot(srcSnap, op)
// 	if err != nil {
// 		return err
// 	}

// 	latestSnapVol := srcSnapshotsAll[len(srcSnapshotsAll)-1]

// 	err = transfer(srcSnap, vol, latestSnapVol)
// 	if err != nil {
// 		// Don't fail here. If it's not possible to perform an optimized refresh, do a generic
// 		// refresh instead.
// 		if errors.Is(err, ErrSnapshotDoesNotMatchIncrementalSource) {
// 			d.logger.Debug("Unable to perform an optimized refresh, doing a generic refresh", logger.Ctx{"err": err})
// 			return genericVFSCopyVolume(d, nil, vol, srcVol, srcSnapshots, true, allowInconsistent, op)
// 		}

// 		return fmt.Errorf("Failed to transfer main volume: %w", err)
// 	}

// 	if srcSnap.IsVMBlock() {
// 		srcFSVol := srcSnap.NewVMBlockFilesystemVolume()
// 		targetFSVol := vol.NewVMBlockFilesystemVolume()
// 		originFSVol := latestSnapVol.NewVMBlockFilesystemVolume()

// 		err = transfer(srcFSVol, targetFSVol, originFSVol)
// 		if err != nil {
// 			// Don't fail here. If it's not possible to perform an optimized refresh, do a generic
// 			// refresh instead.
// 			if errors.Is(err, ErrSnapshotDoesNotMatchIncrementalSource) {
// 				d.logger.Debug("Unable to perform an optimized refresh, doing a generic refresh", logger.Ctx{"err": err})
// 				return genericVFSCopyVolume(d, nil, vol, srcVol, srcSnapshots, true, allowInconsistent, op)
// 			}

// 			return fmt.Errorf("Failed to transfer main volume: %w", err)
// 		}
// 	}

// 	// Restore target volume from main source snapshot.
// 	err = d.RestoreVolume(vol, snapUUID, op)
// 	if err != nil {
// 		return err
// 	}

// 	// Delete temporary source snapshot.
// 	err = d.DeleteVolumeSnapshot(srcSnap, op)
// 	if err != nil {
// 		return err
// 	}

// 	// Delete temporary target snapshot.
// 	targetSnap, err := vol.NewSnapshot(snapUUID)
// 	if err != nil {
// 		return err
// 	}

// 	err = d.DeleteVolumeSnapshot(targetSnap, op)
// 	if err != nil {
// 		return err
// 	}

// 	return nil
// }

// DeleteVolume deletes a volume of the storage device. If any snapshots of the volume remain then
// this function will return an error.
// For image volumes, both filesystem and block volumes will be removed.
func (d *truenas) DeleteVolume(vol Volume, op *operations.Operation) error {
	if vol.volType == VolumeTypeImage {
		// We need to clone vol the otherwise changing `zfs.block_mode`
		// in tmpVol will also change it in vol.
		tmpVol := vol.Clone()

		for _, filesystem := range blockBackedAllowedFilesystems {
			tmpVol.config["block.filesystem"] = filesystem

			err := d.deleteVolume(tmpVol, op)
			if err != nil {
				return err
			}
		}
	}

	return d.deleteVolume(vol, op)
}

func (d *truenas) deleteVolume(vol Volume, op *operations.Operation) error {
	// Check that we have a dataset to delete.
	dataset := d.dataset(vol, false)
	exists, err := d.datasetExists(dataset)
	if err != nil {
		return err
	}

	if exists {
		// Handle clones.
		clones, err := d.getClones(dataset)
		if err != nil {
			return err
		}

		if len(clones) > 0 {
			// Deleted volumes do not need shares
			_ = d.deleteNfsShare(dataset)

			// Move to the deleted path.
			//_, err := subprocess.RunCommand("/proc/self/exe", "forkzfs", "--", "rename", d.dataset(vol, false), d.dataset(vol, true))
			out, err := d.renameDataset(dataset, d.dataset(vol, true), false)
			_ = out
			if err != nil {
				return err
			}
		} else {
			err := d.deleteDatasetRecursive(dataset)
			if err != nil {
				return err
			}
		}
	}

	// Delete the mountpoint if present.
	err = os.Remove(vol.MountPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("Failed to remove '%s': %w", vol.MountPath(), err)
	}

	if vol.contentType == ContentTypeFS {
		// Delete the snapshot storage.
		err = os.RemoveAll(GetVolumeSnapshotDir(d.name, vol.volType, vol.name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("Failed to remove '%s': %w", GetVolumeSnapshotDir(d.name, vol.volType, vol.name), err)
		}

		// TODO: we should probably cleanup using DeleteVolume.
		if needsFsImgVol(vol) {
			fsImgVol := cloneVolAsFsImgVol(vol)
			err := os.Remove(fsImgVol.MountPath())
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("Failed to remove '%s': %w", fsImgVol.MountPath(), err)
			}
		}
	}

	// For VMs, also delete the filesystem dataset.
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.DeleteVolume(fsVol, op)
		if err != nil {
			return err
		}
	}

	return nil
}

// HasVolume indicates whether a specific volume exists on the storage pool.
func (d *truenas) HasVolume(vol Volume) (bool, error) {
	// Check if the dataset exists.
	dataset := d.dataset(vol, false)
	return d.datasetExists(dataset)
}

// commonVolumeRules returns validation rules which are common for pool and volume.
func (d *truenas) commonVolumeRules() map[string]func(value string) error {
	return map[string]func(value string) error{
		"block.filesystem":     validate.Optional(validate.IsOneOf(blockBackedAllowedFilesystems...)),
		"block.mount_options":  validate.IsAny,
		"truenas.block_mode":   validate.Optional(validate.IsBool),
		"zfs.blocksize":        validate.Optional(ValidateZfsBlocksize),
		"zfs.remove_snapshots": validate.Optional(validate.IsBool),
		"zfs.reserve_space":    validate.Optional(validate.IsBool),
		"zfs.use_refquota":     validate.Optional(validate.IsBool),
		"zfs.delegate":         validate.Optional(validate.IsBool),
	}
}

// ValidateVolume validates the supplied volume config.
func (d *truenas) ValidateVolume(vol Volume, removeUnknownKeys bool) error {
	commonRules := d.commonVolumeRules()

	// Disallow block.* settings for regular custom block volumes. These settings only make sense
	// when using custom filesystem volumes. Incus will create the filesystem
	// for these volumes, and use the mount options. When attaching a regular block volume to a VM,
	// these are not mounted by Incus and therefore don't need these config keys.
	if vol.IsVMBlock() || vol.volType == VolumeTypeCustom && vol.contentType == ContentTypeBlock {
		delete(commonRules, "block.filesystem")
		delete(commonRules, "block.mount_options")
	}

	return d.validateVolume(vol, commonRules, removeUnknownKeys)
}

// // UpdateVolume applies config changes to the volume.
func (d *truenas) UpdateVolume(vol Volume, changedConfig map[string]string) error {
	// Mangle the current volume to its old values.
	old := make(map[string]string)
	for k, v := range changedConfig {
		if k == "size" || k == "zfs.use_refquota" || k == "zfs.reserve_space" {
			old[k] = vol.config[k]
			vol.config[k] = v
		}

		if k == "zfs.blocksize" {
			// Convert to bytes.
			sizeBytes, err := units.ParseByteSizeString(v)
			if err != nil {
				return err
			}

			err = d.setBlocksize(vol, sizeBytes)
			if err != nil {
				return err
			}
		}
	}

	defer func() {
		for k, v := range old {
			vol.config[k] = v
		}
	}()

	// If any of the relevant keys changed, re-apply the quota.
	if len(old) != 0 {
		err := d.SetVolumeQuota(vol, vol.ExpandedConfig("size"), false, nil)
		if err != nil {
			return err
		}
	}

	return nil
}

// // GetVolumeUsage returns the disk space used by the volume.
// func (d *zfs) GetVolumeUsage(vol Volume) (int64, error) {
// 	// Determine what key to use.
// 	key := "used"

// 	// If volume isn't snapshot then we can take into account the zfs.use_refquota setting.
// 	// Snapshots should also use the "used" ZFS property because the snapshot usage size represents the CoW
// 	// usage not the size of the snapshot volume.
// 	if !vol.IsSnapshot() {
// 		if util.IsTrue(vol.ExpandedConfig("zfs.use_refquota")) {
// 			key = "referenced"
// 		}

// 		// Shortcut for mounted refquota filesystems.
// 		if key == "referenced" && vol.contentType == ContentTypeFS && linux.IsMountPoint(vol.MountPath()) {
// 			var stat unix.Statfs_t
// 			err := unix.Statfs(vol.MountPath(), &stat)
// 			if err != nil {
// 				return -1, err
// 			}

// 			return int64(stat.Blocks-stat.Bfree) * int64(stat.Bsize), nil
// 		}
// 	}

// 	// Get the current value.
// 	value, err := d.getDatasetProperty(d.dataset(vol, false), key)
// 	if err != nil {
// 		return -1, err
// 	}

// 	// Convert to int.
// 	valueInt, err := strconv.ParseInt(value, 10, 64)
// 	if err != nil {
// 		return -1, err
// 	}

// 	return valueInt, nil
// }

//

// SetVolumeQuota applies a size limit on volume.
// Does nothing if supplied with an empty/zero size for block volumes, and for filesystem volumes removes quota.
func (d *truenas) SetVolumeQuota(vol Volume, size string, allowUnsafeResize bool, op *operations.Operation) error {
	// Convert to bytes.
	sizeBytes, err := units.ParseByteSizeString(size)
	if err != nil {
		return err
	}

	// For VM block files, resize the file if needed.
	if vol.contentType == ContentTypeBlock {
		// Do nothing if size isn't specified.
		if sizeBytes <= 0 {
			return nil
		}

		// rootBlockPath, err := d.GetVolumeDiskPath(vol)
		// if err != nil {
		// 	return err
		// }

		// resized, err := ensureVolumeBlockFile(vol, rootBlockPath, sizeBytes, allowUnsafeResize)
		// if err != nil {
		// 	return err
		// }

		// // Move the GPT alt header to end of disk if needed and resize has taken place (not needed in
		// // unsafe resize mode as it is expected the caller will do all necessary post resize actions
		// // themselves).
		// if vol.IsVMBlock() && resized && !allowUnsafeResize {
		// 	err = d.moveGPTAltHeader(rootBlockPath)
		// 	if err != nil {
		// 		return err
		// 	}
		// }

		return nil
	} else if vol.Type() != VolumeTypeBucket {
		// For non-VM block volumes, set filesystem quota.
		volID, err := d.getVolID(vol.volType, vol.name)
		_ = volID
		if err != nil {
			return err
		}

		// Custom handling for filesystem volume associated with a VM.
		volPath := vol.MountPath()
		if sizeBytes > 0 && vol.volType == VolumeTypeVM && util.PathExists(filepath.Join(volPath, genericVolumeDiskFile)) {
			// Get the size of the VM image.
			blockSize, err := BlockDiskSizeBytes(filepath.Join(volPath, genericVolumeDiskFile))
			if err != nil {
				return err
			}

			// Add that to the requested filesystem size (to ignore it from the quota).
			sizeBytes += blockSize
			d.logger.Debug("Accounting for VM image file size", logger.Ctx{"sizeBytes": sizeBytes})
		}

		//return d.setQuota(vol.MountPath(), volID, sizeBytes)
		return nil
	}

	return nil
}

// se: from driver_dir_volumes.go
// GetVolumeDiskPath returns the location of a disk volume.
func (d *truenas) GetVolumeDiskPath(vol Volume) (string, error) {
	return filepath.Join(vol.MountPath(), genericVolumeDiskFile), nil
}

// ListVolumes returns a list of volumes in storage pool.
func (d *truenas) ListVolumes() ([]Volume, error) {
	vols := make(map[string]Volume)
	_ = vols

	// Get just filesystem and volume datasets, not snapshots.
	// The ZFS driver uses two approaches to indicating block volumes; firstly for VM and image volumes it
	// creates both a filesystem dataset and an associated volume ending in zfsBlockVolSuffix.
	// However for custom block volumes it does not also end the volume name in zfsBlockVolSuffix (unlike the
	// LVM and Ceph drivers), so we must also retrieve the dataset type here and look for "volume" types
	// which also indicate this is a block volume.
	//cmd := exec.Command("zfs", "list", "-H", "-o", "name,type,incus:content_type", "-r", "-t", "filesystem,volume", d.config["zfs.pool_name"])
	out, err := d.runTool("dataset", "list", "-H", "-o", "name,type,incus:content_type", "-r" /*"-t","filesystem,volume",*/, d.config["truenas.dataset"])
	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(strings.NewReader(out))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Splitting fields on tab should be safe as ZFS doesn't appear to allow tabs in dataset names.
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			return nil, fmt.Errorf("Unexpected volume line %q", line)
		}

		zfsVolName := parts[0]
		zfsContentType := parts[1]
		incusContentType := parts[2]

		var volType VolumeType
		var volName string

		for _, volumeType := range d.Info().VolumeTypes {
			prefix := fmt.Sprintf("%s/%s/", d.config["truenas.dataset"], volumeType)
			if strings.HasPrefix(zfsVolName, prefix) {
				volType = volumeType
				volName = strings.TrimPrefix(zfsVolName, prefix)
			}
		}

		if volType == "" {
			d.logger.Debug("Ignoring unrecognised volume type", logger.Ctx{"name": zfsVolName})
			continue // Ignore unrecognised volume.
		}

		// Detect if a volume is block content type using only the dataset type.
		isBlock := zfsContentType == "volume"

		if volType == VolumeTypeVM && !isBlock {
			continue // Ignore VM filesystem volumes as we will just return the VM's block volume.
		}

		contentType := ContentTypeFS
		if isBlock {
			contentType = ContentTypeBlock
		}

		if volType == VolumeTypeCustom && isBlock && strings.HasSuffix(volName, zfsISOVolSuffix) {
			contentType = ContentTypeISO
			volName = strings.TrimSuffix(volName, zfsISOVolSuffix)
		} else if volType == VolumeTypeVM || isBlock {
			volName = strings.TrimSuffix(volName, zfsBlockVolSuffix)
		}

		// If a new volume has been found, or the volume will replace an existing image filesystem volume
		// then proceed to add the volume to the map. We allow image volumes to overwrite existing
		// filesystem volumes of the same name so that for VM images we only return the block content type
		// volume (so that only the single "logical" volume is returned).
		existingVol, foundExisting := vols[volName]
		if !foundExisting || (existingVol.Type() == VolumeTypeImage && existingVol.ContentType() == ContentTypeFS) {
			v := NewVolume(d, d.name, volType, contentType, volName, make(map[string]string), d.config)

			if isBlock {
				// Get correct content type from incus:content_type property.
				if incusContentType != "-" {
					v.contentType = ContentType(incusContentType)
				}

				if v.contentType == ContentTypeBlock {
					v.SetMountFilesystemProbe(true)
				}
			}

			vols[volName] = v
			continue
		}

		return nil, fmt.Errorf("Unexpected duplicate volume %q found", volName)
	}

	volList := make([]Volume, 0, len(vols))
	for _, v := range vols {
		volList = append(volList, v)
	}

	return volList, nil
}

func (d *truenas) activateAndMountFsImg(vol Volume, op *operations.Operation) error {

	revert := revert.New()
	defer revert.Fail()

	// mount underlying dataset, then loop mount the root.img
	fsImgVol := cloneVolAsFsImgVol(vol)

	err := d.MountVolume(fsImgVol, op)
	if err != nil {
		return err
	}
	revert.Add(func() {
		_, _ = d.UnmountVolume(fsImgVol, false, op)
	})

	// We expect the filler to copy the VM image into this path.
	rootBlockPath, err := d.GetVolumeDiskPath(fsImgVol)
	if err != nil {
		return err
	}

	fsType, err := fsProbe(rootBlockPath)
	if err != nil {
		return fmt.Errorf("Failed probing filesystem: %w", err)
	}
	if fsType == "" {
		// if we couln't probe it, we probably can't mount it, but may as well give it a whirl
		fsType = vol.ConfigBlockFilesystem()
	}

	loopDevPath, err := loopDeviceSetup(rootBlockPath)
	if err != nil {
		return err
	}
	revert.Add(func() {
		loopDeviceAutoDetach(loopDevPath)
	})

	mountPath := vol.MountPath()

	//var volOptions []string
	volOptions := strings.Split(vol.ConfigBlockMountOptions(), ",")

	mountFlags, mountOptions := linux.ResolveMountOptions(volOptions)
	_ = mountFlags
	err = TryMount(loopDevPath, mountPath, fsType, mountFlags, mountOptions)
	if err != nil {
		defer func() { _ = loopDeviceAutoDetach(loopDevPath) }()
		return err
	}
	d.logger.Debug("Mounted TrueNAS volume", logger.Ctx{"volName": vol.name, "dev": rootBlockPath, "path": mountPath, "options": mountOptions})

	revert.Success()

	return nil
}

func (d *truenas) mountNfsDataset(vol Volume) error {

	err := vol.EnsureMountPath()
	if err != nil {
		return err
	}

	dataset := d.dataset(vol, false)

	var volOptions []string

	//note: to implement getDatasetProperties, we'd like `truenas-admin dataset inspect` to be implemented
	atime, _ := d.getDatasetProperty(dataset, "atime")
	if atime == "off" {
		volOptions = append(volOptions, "noatime")
	}

	host := d.config["truenas.host"]
	if host == "" {
		return fmt.Errorf("`truenas.host` must be specified")
	}

	ip4and6, err := net.LookupIP(host)
	if err != nil {
		return err
	}

	// NFS
	volOptions = append(volOptions, "vers=4.2")                  // TODO: decide on default options
	volOptions = append(volOptions, "addr="+ip4and6[0].String()) // TODO: pick ip4 or ip6

	mountFlags, mountOptions := linux.ResolveMountOptions(volOptions)
	mountPath := vol.MountPath()

	remotePath := fmt.Sprintf("%s:/mnt/%s", host, dataset)

	// Mount the dataset.
	err = TryMount(remotePath, mountPath, "nfs", mountFlags, mountOptions) // TODO: if local we want to bind mount.

	if err != nil {
		// try once more, after re-creating the share.
		err = d.createNfsShare(dataset)
		if err != nil {
			return err
		}
		err = TryMount(remotePath, mountPath, "nfs", mountFlags, mountOptions)
		if err != nil {
			return err
		}
	}

	d.logger.Debug("Mounted TrueNAS dataset", logger.Ctx{"volName": vol.name, "host": host, "dev": dataset, "path": mountPath})

	return nil
}

// MountVolume mounts a volume and increments ref counter. Please call UnmountVolume() when done with the volume.
func (d *truenas) MountVolume(vol Volume, op *operations.Operation) error {
	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	revert := revert.New()
	defer revert.Fail()

	if vol.contentType == ContentTypeFS || isFsImgVol(vol) || vol.IsVMBlock() {

		// handle an FS mount

		mountPath := vol.MountPath()
		if !linux.IsMountPoint(mountPath) {

			if needsFsImgVol(vol) {

				// mount underlying fs, then create a loop device for the fs-img, and mount that
				err = d.activateAndMountFsImg(vol, op)
				if err != nil {
					return err
				}

			} else {

				// otherwise, we can just NFS mount a dataset
				err = d.mountNfsDataset(vol)
				if err != nil {
					return err
				}
			}
		}

	} else if vol.contentType == ContentTypeBlock || vol.contentType == ContentTypeISO {
		/*
			Like the spoon, there is no block volume.

			For VMs, mount the filesystem volume. This essentially has the effect of double-mounting the FS volume
			when we are mounting the block device. This prevents the FS volume being unmounted prematurely.

			Its important to mount the block volume and then its underlying "config" filesystem volume because
			vol.NewVMBlockFilesystemVolume is used to to mount the VM's config without necessarily mounting the "block" volume,
			and if we don't explicitly mount it, then MountTask will blindly unmount our block volume.
		*/
		if vol.IsVMBlock() {
			fsVol := vol.NewVMBlockFilesystemVolume()
			fsVol.config["volatile.truenas.fs-img"] = "true" // bit of a hack to get the fs-mounter to mount it instead of loop it.
			err = d.MountVolume(fsVol, op)
			if err != nil {
				return err
			}
		} // PS: not 100% sure what to do about ISOs yet.
	}

	// now, if we were a VM block we also need to mount the config filesystem
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		//fsVol.config["volatile.truenas.fs-img"] = "true" // bit of a hack to get the fs-mounter to mount it instead of loop it.
		err = d.MountVolume(fsVol, op)
		if err != nil {
			return err
		}
	} // PS: not 100% sure what to do about ISOs yet.

	vol.MountRefCountIncrement() // From here on it is up to caller to call UnmountVolume() when done.
	revert.Success()
	return nil
}

func (d *truenas) deactivateVolume(vol Volume, op *operations.Operation) (bool, error) {
	ourUnmount := true

	// need to unlink the loop
	// mount underlying dataset, then loop mount the root.img
	// we need to mount the underlying dataset
	fsImgVol := cloneVolAsFsImgVol(vol)

	// We expect the filler to copy the VM image into this path.
	rootBlockPath, err := d.GetVolumeDiskPath(fsImgVol)
	if err != nil {
		return false, err
	}
	loopDevPath, err := loopDeviceSetup(rootBlockPath)
	if err != nil {
		return false, err
	}
	err = loopDeviceAutoDetach(loopDevPath)
	if err != nil {
		return false, err
	}

	// and then unmount the root.img dataset

	_, err = d.UnmountVolume(fsImgVol, false, op)
	if err != nil {
		return false, err
	}

	return ourUnmount, nil
}

// UnmountVolume unmounts volume if mounted and not in use. Returns true if this unmounted the volume.
// keepBlockDev indicates if backing block device should be not be deactivated when volume is unmounted.
func (d *truenas) UnmountVolume(vol Volume, keepBlockDev bool, op *operations.Operation) (bool, error) {
	unlock, err := vol.MountLock()
	if err != nil {
		return false, err
	}

	defer unlock()

	ourUnmount := false
	dataset := d.dataset(vol, false)
	mountPath := vol.MountPath()

	refCount := vol.MountRefCountDecrement()

	if refCount > 0 {
		d.logger.Debug("Skipping unmount as in use", logger.Ctx{"volName": vol.name, "refCount": refCount})
		return false, ErrInUse
	}

	if keepBlockDev {
		d.logger.Debug("keepBlockDevTrue", logger.Ctx{"volName": vol.name, "refCount": refCount})
	}

	if (vol.contentType == ContentTypeFS || vol.IsVMBlock() || isFsImgVol(vol)) && linux.IsMountPoint(mountPath) {

		// Unmount the dataset.
		err = TryUnmount(mountPath, 0)
		if err != nil {
			return false, err
		}
		ourUnmount = true

		// if we're a loop mounted volume...
		if needsFsImgVol(vol) {

			// then we've unmounted the volume

			d.logger.Debug("Unmounted TrueNAS volume", logger.Ctx{"volName": vol.name, "host": d.config["truenas.host"], "dataset": dataset, "path": mountPath})

			// now we can take down the loop and the fs-img dataset
			_, err = d.deactivateVolume(vol, op)
			if err != nil {
				return false, err
			}

		} else {
			// otherwise, we're just a regular dataset mount.
			d.logger.Debug("Unmounted TrueNAS dataset", logger.Ctx{"volName": vol.name, "host": d.config["truenas.host"], "dataset": dataset, "path": mountPath})
		}

	}

	if vol.contentType == ContentTypeBlock || vol.contentType == ContentTypeISO {
		// For VMs and ISOs, unmount the filesystem volume.
		if vol.IsVMBlock() {
			fsVol := vol.NewVMBlockFilesystemVolume()
			ourUnmount, err = d.UnmountVolume(fsVol, false, op)
			if err != nil {
				return false, err
			}
		}
	}

	return ourUnmount, nil
}

// RenameVolume renames a volume and its snapshots.
func (d *truenas) RenameVolume(vol Volume, newVolName string, op *operations.Operation) error {
	newVol := NewVolume(d, d.name, vol.volType, vol.contentType, newVolName, vol.config, vol.poolConfig)

	// Revert handling.
	revert := revert.New()
	defer revert.Fail()

	// First rename the VFS paths.
	err := genericVFSRenameVolume(d, vol, newVolName, op)
	if err != nil {
		return err
	}

	revert.Add(func() {
		_ = genericVFSRenameVolume(d, newVol, vol.name, op)
	})

	// Rename the ZFS datasets.
	//_, err = subprocess.RunCommand("zfs", "rename", d.dataset(vol, false), d.dataset(newVol, false))
	out, err := d.renameDataset(d.dataset(vol, false), d.dataset(newVol, false), true)
	_ = out
	if err != nil {
		return err
	}

	revert.Add(func() {
		//_, _ = subprocess.RunCommand("zfs", "rename", d.dataset(newVol, false), d.dataset(vol, false))
		_, _ = d.renameDataset(d.dataset(newVol, false), d.dataset(vol, false), true)

	})

	// All done.
	revert.Success()

	return nil
}

// // MigrateVolume sends a volume for migration.
// func (d *zfs) MigrateVolume(vol Volume, conn io.ReadWriteCloser, volSrcArgs *localMigration.VolumeSourceArgs, op *operations.Operation) error {
// 	if !volSrcArgs.AllowInconsistent && vol.contentType == ContentTypeFS && vol.IsBlockBacked() {
// 		// When migrating using zfs volumes (not datasets), ensure that the filesystem is synced
// 		// otherwise the source and target volumes may differ. Tests have shown that only calling
// 		// os.SyncFS() doesn't suffice. A freeze and unfreeze is needed.
// 		err := vol.MountTask(func(mountPath string, op *operations.Operation) error {
// 			unfreezeFS, err := d.filesystemFreeze(mountPath)
// 			if err != nil {
// 				return err
// 			}

// 			return unfreezeFS()
// 		}, op)
// 		if err != nil {
// 			return err
// 		}
// 	}

// 	// Handle simple rsync and block_and_rsync through generic.
// 	if volSrcArgs.MigrationType.FSType == migration.MigrationFSType_RSYNC || volSrcArgs.MigrationType.FSType == migration.MigrationFSType_BLOCK_AND_RSYNC {
// 		// If volume is filesystem type, create a fast snapshot to ensure migration is consistent.
// 		// TODO add support for temporary snapshots of block volumes here.
// 		if vol.contentType == ContentTypeFS && !vol.IsSnapshot() {
// 			snapshotPath, cleanup, err := d.readonlySnapshot(vol)
// 			if err != nil {
// 				return err
// 			}

// 			// Clean up the snapshot.
// 			defer cleanup()

// 			// Set the path of the volume to the path of the fast snapshot so the migration reads from there instead.
// 			vol.mountCustomPath = snapshotPath
// 		}

// 		return genericVFSMigrateVolume(d, d.state, vol, conn, volSrcArgs, op)
// 	} else if volSrcArgs.MigrationType.FSType != migration.MigrationFSType_ZFS {
// 		return ErrNotSupported
// 	}

// 	// Handle zfs send/receive migration.
// 	if volSrcArgs.MultiSync || volSrcArgs.FinalSync {
// 		// This is not needed if the migration is performed using zfs send/receive.
// 		return fmt.Errorf("MultiSync should not be used with optimized migration")
// 	}

// 	var srcMigrationHeader *ZFSMetaDataHeader

// 	// The target will validate the GUIDs and if successful proceed with the refresh.
// 	if slices.Contains(volSrcArgs.MigrationType.Features, migration.ZFSFeatureMigrationHeader) {
// 		snapshots, err := d.VolumeSnapshots(vol, op)
// 		if err != nil {
// 			return err
// 		}

// 		// Fill the migration header with the snapshot names and dataset GUIDs.
// 		srcMigrationHeader, err = d.datasetHeader(vol, snapshots)
// 		if err != nil {
// 			return err
// 		}

// 		headerJSON, err := json.Marshal(srcMigrationHeader)
// 		if err != nil {
// 			return fmt.Errorf("Failed encoding ZFS migration header: %w", err)
// 		}

// 		// Send the migration header to the target.
// 		_, err = conn.Write(headerJSON)
// 		if err != nil {
// 			return fmt.Errorf("Failed sending ZFS migration header: %w", err)
// 		}

// 		err = conn.Close() //End the frame.
// 		if err != nil {
// 			return fmt.Errorf("Failed closing ZFS migration header frame: %w", err)
// 		}
// 	}

// 	// If we haven't negotiated zvol support, ensure volume is not a zvol.
// 	if !slices.Contains(volSrcArgs.MigrationType.Features, migration.ZFSFeatureZvolFilesystems) && d.isBlockBacked(vol) {
// 		return fmt.Errorf("Filesystem zvol detected in source but target does not support receiving zvols")
// 	}

// 	incrementalStream := true
// 	var migrationHeader ZFSMetaDataHeader

// 	if volSrcArgs.Refresh && slices.Contains(volSrcArgs.MigrationType.Features, migration.ZFSFeatureMigrationHeader) {
// 		buf, err := io.ReadAll(conn)
// 		if err != nil {
// 			return fmt.Errorf("Failed reading ZFS migration header: %w", err)
// 		}

// 		err = json.Unmarshal(buf, &migrationHeader)
// 		if err != nil {
// 			return fmt.Errorf("Failed decoding ZFS migration header: %w", err)
// 		}

// 		// If the target has no snapshots we cannot use incremental streams and will do a normal copy operation instead.
// 		if len(migrationHeader.SnapshotDatasets) == 0 {
// 			incrementalStream = false
// 			volSrcArgs.Refresh = false
// 		}

// 		volSrcArgs.Snapshots = []string{}

// 		// Override volSrcArgs.Snapshots to only include snapshots which need to be sent.
// 		if !volSrcArgs.VolumeOnly {
// 			for _, srcDataset := range srcMigrationHeader.SnapshotDatasets {
// 				found := false

// 				for _, dstDataset := range migrationHeader.SnapshotDatasets {
// 					if srcDataset.GUID == dstDataset.GUID {
// 						found = true
// 						break
// 					}
// 				}

// 				if !found {
// 					volSrcArgs.Snapshots = append(volSrcArgs.Snapshots, srcDataset.Name)
// 				}
// 			}
// 		}
// 	}

// 	return d.migrateVolumeOptimized(vol, conn, volSrcArgs, incrementalStream, op)
// }

// func (d *zfs) migrateVolumeOptimized(vol Volume, conn io.ReadWriteCloser, volSrcArgs *localMigration.VolumeSourceArgs, incremental bool, op *operations.Operation) error {
// 	if vol.IsVMBlock() {
// 		fsVol := vol.NewVMBlockFilesystemVolume()
// 		err := d.migrateVolumeOptimized(fsVol, conn, volSrcArgs, incremental, op)
// 		if err != nil {
// 			return err
// 		}
// 	}

// 	// Handle zfs send/receive migration.
// 	var finalParent string

// 	// Transfer the snapshots first.
// 	for i, snapName := range volSrcArgs.Snapshots {
// 		snapshot, _ := vol.NewSnapshot(snapName)

// 		// Figure out parent and current subvolumes.
// 		parent := ""
// 		if i == 0 && volSrcArgs.Refresh {
// 			snapshots, err := vol.Snapshots(op)
// 			if err != nil {
// 				return err
// 			}

// 			for k, snap := range snapshots {
// 				if k == 0 {
// 					continue
// 				}

// 				if snap.name == fmt.Sprintf("%s/%s", vol.name, snapName) {
// 					parent = d.dataset(snapshots[k-1], false)
// 					break
// 				}
// 			}
// 		} else if i > 0 {
// 			oldSnapshot, _ := vol.NewSnapshot(volSrcArgs.Snapshots[i-1])
// 			parent = d.dataset(oldSnapshot, false)
// 		}

// 		// Setup progress tracking.
// 		var wrapper *ioprogress.ProgressTracker
// 		if volSrcArgs.TrackProgress {
// 			wrapper = localMigration.ProgressTracker(op, "fs_progress", snapshot.name)
// 		}

// 		// Send snapshot to recipient (ensure local snapshot volume is mounted if needed).
// 		err := d.sendDataset(d.dataset(snapshot, false), parent, volSrcArgs, conn, wrapper)
// 		if err != nil {
// 			return err
// 		}

// 		finalParent = d.dataset(snapshot, false)
// 	}

// 	// Setup progress tracking.
// 	var wrapper *ioprogress.ProgressTracker
// 	if volSrcArgs.TrackProgress {
// 		wrapper = localMigration.ProgressTracker(op, "fs_progress", vol.name)
// 	}

// 	srcSnapshot := d.dataset(vol, false)
// 	if !vol.IsSnapshot() {
// 		// Create a temporary read-only snapshot.
// 		srcSnapshot = fmt.Sprintf("%s@migration-%s", d.dataset(vol, false), uuid.New().String())
// 		_, err := subprocess.RunCommand("zfs", "snapshot", "-r", srcSnapshot)
// 		if err != nil {
// 			return err
// 		}

// 		defer func() {
// 			// Delete snapshot (or mark for deferred deletion if cannot be deleted currently).
// 			_, err := subprocess.RunCommand("zfs", "destroy", "-r", "-d", srcSnapshot)
// 			if err != nil {
// 				d.logger.Warn("Failed deleting temporary snapshot for migration", logger.Ctx{"snapshot": srcSnapshot, "err": err})
// 			}
// 		}()
// 	}

// 	// Get parent snapshot of the main volume which can then be used to send an incremental stream.
// 	if volSrcArgs.Refresh && incremental {
// 		localSnapshots, err := vol.Snapshots(op)
// 		if err != nil {
// 			return err
// 		}

// 		if len(localSnapshots) > 0 {
// 			finalParent = d.dataset(localSnapshots[len(localSnapshots)-1], false)
// 		}
// 	}

// 	// Send the volume itself.
// 	err := d.sendDataset(srcSnapshot, finalParent, volSrcArgs, conn, wrapper)
// 	if err != nil {
// 		return err
// 	}

// 	return nil
// }

// func (d *zfs) readonlySnapshot(vol Volume) (string, revert.Hook, error) {
// 	revert := revert.New()
// 	defer revert.Fail()

// 	poolPath := GetPoolMountPath(d.name)
// 	tmpDir, err := os.MkdirTemp(poolPath, "backup.")
// 	if err != nil {
// 		return "", nil, err
// 	}

// 	revert.Add(func() {
// 		_ = os.RemoveAll(tmpDir)
// 	})

// 	err = os.Chmod(tmpDir, 0100)
// 	if err != nil {
// 		return "", nil, err
// 	}

// 	snapshotOnlyName := fmt.Sprintf("temp_ro-%s", uuid.New().String())

// 	snapVol, err := vol.NewSnapshot(snapshotOnlyName)
// 	if err != nil {
// 		return "", nil, err
// 	}

// 	snapshotDataset := fmt.Sprintf("%s@%s", d.dataset(vol, false), snapshotOnlyName)

// 	// Create a temporary snapshot.
// 	_, err = subprocess.RunCommand("zfs", "snapshot", "-r", snapshotDataset)
// 	if err != nil {
// 		return "", nil, err
// 	}

// 	revert.Add(func() {
// 		// Delete snapshot (or mark for deferred deletion if cannot be deleted currently).
// 		_, err := subprocess.RunCommand("zfs", "destroy", "-r", "-d", snapshotDataset)
// 		if err != nil {
// 			d.logger.Warn("Failed deleting read-only snapshot", logger.Ctx{"snapshot": snapshotDataset, "err": err})
// 		}
// 	})

// 	hook, err := d.mountVolumeSnapshot(snapVol, snapshotDataset, tmpDir, nil)
// 	if err != nil {
// 		return "", nil, err
// 	}

// 	revert.Add(hook)

// 	cleanup := revert.Clone().Fail
// 	revert.Success()
// 	return tmpDir, cleanup, nil
// }

// // BackupVolume creates an exported version of a volume.
// func (d *zfs) BackupVolume(vol Volume, tarWriter *instancewriter.InstanceTarWriter, optimized bool, snapshots []string, op *operations.Operation) error {
// 	// Handle the non-optimized tarballs through the generic packer.
// 	if !optimized {
// 		// Because the generic backup method will not take a consistent backup if files are being modified
// 		// as they are copied to the tarball, as ZFS allows us to take a quick snapshot without impacting
// 		// the parent volume we do so here to ensure the backup taken is consistent.
// 		if vol.contentType == ContentTypeFS && !d.isBlockBacked(vol) {
// 			snapshotPath, cleanup, err := d.readonlySnapshot(vol)
// 			if err != nil {
// 				return err
// 			}

// 			// Clean up the snapshot.
// 			defer cleanup()

// 			// Set the path of the volume to the path of the fast snapshot so the migration reads from there instead.
// 			vol.mountCustomPath = snapshotPath
// 		}

// 		return genericVFSBackupVolume(d, vol, tarWriter, snapshots, op)
// 	}

// 	// Optimized backup.

// 	if len(snapshots) > 0 {
// 		// Check requested snapshot match those in storage.
// 		err := vol.SnapshotsMatch(snapshots, op)
// 		if err != nil {
// 			return err
// 		}
// 	}

// 	// Backup VM config volumes first.
// 	if vol.IsVMBlock() {
// 		fsVol := vol.NewVMBlockFilesystemVolume()
// 		err := d.BackupVolume(fsVol, tarWriter, optimized, snapshots, op)
// 		if err != nil {
// 			return err
// 		}
// 	}

// 	// Handle the optimized tarballs.
// 	sendToFile := func(path string, parent string, fileName string) error {
// 		// Prepare zfs send arguments.
// 		args := []string{"send"}

// 		// Check if nesting is required.
// 		if d.needsRecursion(path) {
// 			args = append(args, "-R")

// 			if zfsRaw {
// 				args = append(args, "-w")
// 			}
// 		}

// 		if parent != "" {
// 			args = append(args, "-i", parent)
// 		}

// 		args = append(args, path)

// 		// Create temporary file to store output of ZFS send.
// 		backupsPath := internalUtil.VarPath("backups")
// 		tmpFile, err := os.CreateTemp(backupsPath, fmt.Sprintf("%s_zfs", backup.WorkingDirPrefix))
// 		if err != nil {
// 			return fmt.Errorf("Failed to open temporary file for ZFS backup: %w", err)
// 		}

// 		defer func() { _ = tmpFile.Close() }()
// 		defer func() { _ = os.Remove(tmpFile.Name()) }()

// 		// Write the subvolume to the file.
// 		d.logger.Debug("Generating optimized volume file", logger.Ctx{"sourcePath": path, "file": tmpFile.Name(), "name": fileName})

// 		// Write the subvolume to the file.
// 		err = subprocess.RunCommandWithFds(context.TODO(), nil, tmpFile, "zfs", args...)
// 		if err != nil {
// 			return err
// 		}

// 		// Get info (importantly size) of the generated file for tarball header.
// 		tmpFileInfo, err := os.Lstat(tmpFile.Name())
// 		if err != nil {
// 			return err
// 		}

// 		err = tarWriter.WriteFile(fileName, tmpFile.Name(), tmpFileInfo, false)
// 		if err != nil {
// 			return err
// 		}

// 		return tmpFile.Close()
// 	}

// 	// Handle snapshots.
// 	finalParent := ""
// 	if len(snapshots) > 0 {
// 		for i, snapName := range snapshots {
// 			snapshot, _ := vol.NewSnapshot(snapName)

// 			// Figure out parent and current subvolumes.
// 			parent := ""
// 			if i > 0 {
// 				oldSnapshot, _ := vol.NewSnapshot(snapshots[i-1])
// 				parent = d.dataset(oldSnapshot, false)
// 			}

// 			// Make a binary zfs backup.
// 			prefix := "snapshots"
// 			fileName := fmt.Sprintf("%s.bin", snapName)
// 			if vol.volType == VolumeTypeVM {
// 				prefix = "virtual-machine-snapshots"
// 				if vol.contentType == ContentTypeFS {
// 					fileName = fmt.Sprintf("%s-config.bin", snapName)
// 				}
// 			} else if vol.volType == VolumeTypeCustom {
// 				prefix = "volume-snapshots"
// 			}

// 			target := fmt.Sprintf("backup/%s/%s", prefix, fileName)
// 			err := sendToFile(d.dataset(snapshot, false), parent, target)
// 			if err != nil {
// 				return err
// 			}

// 			finalParent = d.dataset(snapshot, false)
// 		}
// 	}

// 	// Create a temporary read-only snapshot.
// 	srcSnapshot := fmt.Sprintf("%s@backup-%s", d.dataset(vol, false), uuid.New().String())
// 	_, err := subprocess.RunCommand("zfs", "snapshot", "-r", srcSnapshot)
// 	if err != nil {
// 		return err
// 	}

// 	defer func() {
// 		// Delete snapshot (or mark for deferred deletion if cannot be deleted currently).
// 		_, err := subprocess.RunCommand("zfs", "destroy", "-r", "-d", srcSnapshot)
// 		if err != nil {
// 			d.logger.Warn("Failed deleting temporary snapshot for backup", logger.Ctx{"snapshot": srcSnapshot, "err": err})
// 		}
// 	}()

// 	// Dump the container to a file.
// 	fileName := "container.bin"
// 	if vol.volType == VolumeTypeVM {
// 		if vol.contentType == ContentTypeFS {
// 			fileName = "virtual-machine-config.bin"
// 		} else {
// 			fileName = "virtual-machine.bin"
// 		}
// 	} else if vol.volType == VolumeTypeCustom {
// 		fileName = "volume.bin"
// 	}

// 	err = sendToFile(srcSnapshot, finalParent, fmt.Sprintf("backup/%s", fileName))
// 	if err != nil {
// 		return err
// 	}

// 	return nil
// }

// CreateVolumeSnapshot creates a snapshot of a volume.
func (d *truenas) CreateVolumeSnapshot(vol Volume, op *operations.Operation) error {
	parentName, _, _ := api.GetParentAndSnapshotName(vol.name)

	// Revert handling.
	revert := revert.New()
	defer revert.Fail()

	// Create the parent directory.
	err := createParentSnapshotDirIfMissing(d.name, vol.volType, parentName)
	if err != nil {
		return err
	}

	// Create snapshot directory.
	err = vol.EnsureMountPath()
	if err != nil {
		return err
	}

	if vol.IsVMBlock() {
		/*
			We want to ensure the current state is flushed to the server before snapping.

			Incus will Freeze the Instance before the snapshot, but if its a VM it won't Sync the FS
			correctly as it targets the ./rootfs as used by lxc

			In future, a better solution may be to correct the Freeze/Unfreeze logic to figure out to
			use the VM's filesystem.

			Ideally, this whole function needs to return ASAP so that the VM will be unfrozen ASAP
		*/
		volMountPath := GetVolumeMountPath(vol.pool, vol.volType, parentName)
		if linux.IsMountPoint(volMountPath) {
			err := linux.SyncFS(volMountPath)
			if err != nil {
				return fmt.Errorf("Failed syncing filesystem %q: %w", volMountPath, err)
			}
		}
	}

	// Make the snapshot.
	dataset := d.dataset(vol, false)
	err = d.createSnapshot(dataset, false)
	if err != nil {
		return err
	}

	revert.Add(func() { _ = d.DeleteVolumeSnapshot(vol, op) })

	// All done.
	revert.Success()

	return nil
}

// DeleteVolumeSnapshot removes a snapshot from the storage device.
func (d *truenas) DeleteVolumeSnapshot(vol Volume, op *operations.Operation) error {
	parentName, _, _ := api.GetParentAndSnapshotName(vol.name)

	// Handle clones.
	clones, err := d.getClones(d.dataset(vol, false))
	if err != nil {
		return err
	}

	if len(clones) > 0 {
		// Move to the deleted path.
		out, err := d.renameSnapshot(d.dataset(vol, false), d.dataset(vol, true))

		_ = out
		if err != nil {
			return err
		}
	} else {
		// Delete the snapshot.
		dataset := d.dataset(vol, false)
		out, err := d.runTool("snapshot", "delete", "-r", dataset)
		_ = out
		if err != nil {
			return err
		}
	}

	// Delete the mountpoint.
	err = os.Remove(vol.MountPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("Failed to remove '%s': %w", vol.MountPath(), err)
	}

	// Remove the parent snapshot directory if this is the last snapshot being removed.
	err = deleteParentSnapshotDirIfEmpty(d.name, vol.volType, parentName)
	if err != nil {
		return err
	}

	return nil
}

// MountVolumeSnapshot simulates mounting a volume snapshot.
func (d *truenas) MountVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	unlock, err := snapVol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	_, err = d.mountVolumeSnapshot(snapVol, d.dataset(snapVol, false), snapVol.MountPath(), op)
	if err != nil {
		return err
	}

	snapVol.MountRefCountIncrement() // From here on it is up to caller to call UnmountVolumeSnapshot() when done.
	return nil
}

func (d *truenas) mountVolumeSnapshot(snapVol Volume, snapshotDataset string, mountPath string, op *operations.Operation) (revert.Hook, error) {
	revert := revert.New()
	defer revert.Fail()

	// Check if filesystem volume already mounted.
	if snapVol.contentType == ContentTypeFS && !d.isBlockBacked(snapVol) {
		if !linux.IsMountPoint(mountPath) {
			err := snapVol.EnsureMountPath()
			if err != nil {
				return nil, err
			}

			// Mount the snapshot directly (not possible through tools).
			err = TryMount(snapshotDataset, mountPath, "zfs", unix.MS_RDONLY, "")
			if err != nil {
				return nil, err
			}

			d.logger.Debug("Mounted ZFS snapshot dataset", logger.Ctx{"dev": snapshotDataset, "path": mountPath})
		}
	} else {
		// snipped.
		return nil, fmt.Errorf("contentType == ContentTypeBlock not implemented")
	}

	d.logger.Debug("Mounted TrueNAS snapshot dataset", logger.Ctx{"dev": snapshotDataset, "path": mountPath})

	revert.Add(func() {
		_, err := forceUnmount(mountPath)
		if err != nil {
			return
		}

		d.logger.Debug("Unmounted TrueNAS snapshot dataset", logger.Ctx{"dev": snapshotDataset, "path": mountPath})
	})

	cleanup := revert.Clone().Fail
	revert.Success()
	return cleanup, nil
}

// // UnmountVolume simulates unmounting a volume snapshot.
// func (d *zfs) UnmountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
// 	unlock, err := snapVol.MountLock()
// 	if err != nil {
// 		return false, err
// 	}

// 	defer unlock()

// 	ourUnmount := false
// 	mountPath := snapVol.MountPath()
// 	snapshotDataset := d.dataset(snapVol, false)

// 	refCount := snapVol.MountRefCountDecrement()

// 	// For block devices, we make them disappear.
// 	if snapVol.contentType == ContentTypeBlock || snapVol.contentType == ContentTypeFS && d.isBlockBacked(snapVol) {
// 		// For VMs, also mount the filesystem dataset.
// 		if snapVol.IsVMBlock() {
// 			fsSnapVol := snapVol.NewVMBlockFilesystemVolume()
// 			ourUnmount, err = d.UnmountVolumeSnapshot(fsSnapVol, op)
// 			if err != nil {
// 				return false, err
// 			}
// 		}

// 		if snapVol.contentType == ContentTypeFS && d.isBlockBacked(snapVol) && linux.IsMountPoint(mountPath) {
// 			if refCount > 0 {
// 				d.logger.Debug("Skipping unmount as in use", logger.Ctx{"volName": snapVol.name, "refCount": refCount})
// 				return false, ErrInUse
// 			}

// 			_, err := forceUnmount(mountPath)
// 			if err != nil {
// 				return false, err
// 			}

// 			d.logger.Debug("Unmounted ZFS snapshot dataset", logger.Ctx{"dev": snapshotDataset, "path": mountPath})
// 			ourUnmount = true

// 			parent, snapshotOnlyName, _ := api.GetParentAndSnapshotName(snapVol.Name())
// 			parentVol := NewVolume(d, d.Name(), snapVol.volType, snapVol.contentType, parent, snapVol.config, snapVol.poolConfig)
// 			parentDataset := d.dataset(parentVol, false)
// 			dataset := fmt.Sprintf("%s_%s%s", parentDataset, snapshotOnlyName, tmpVolSuffix)

// 			exists, err := d.datasetExists(dataset)
// 			if err != nil {
// 				return true, fmt.Errorf("Failed to check existence of temporary ZFS snapshot volume %q: %w", dataset, err)
// 			}

// 			if exists {
// 				err = d.deleteDatasetRecursive(dataset)
// 				if err != nil {
// 					return true, err
// 				}
// 			}
// 		}

// 		parent, _, _ := api.GetParentAndSnapshotName(snapVol.Name())
// 		parentVol := NewVolume(d, d.Name(), snapVol.volType, snapVol.contentType, parent, snapVol.config, snapVol.poolConfig)
// 		parentDataset := d.dataset(parentVol, false)

// 		current, err := d.getDatasetProperty(parentDataset, "snapdev")
// 		if err != nil {
// 			return false, err
// 		}

// 		if current == "visible" {
// 			if refCount > 0 {
// 				d.logger.Debug("Skipping unmount as in use", logger.Ctx{"volName": snapVol.name, "refCount": refCount})
// 				return false, ErrInUse
// 			}

// 			err := d.setDatasetProperties(parentDataset, "snapdev=hidden")
// 			if err != nil {
// 				return false, err
// 			}

// 			d.logger.Debug("Deactivated ZFS snapshot volume", logger.Ctx{"dev": snapshotDataset})

// 			// Ensure snap volume parent is deactivated in case we activated it when mounting snapshot.
// 			_, err = d.UnmountVolume(parentVol, false, op)
// 			if err != nil {
// 				return false, err
// 			}

// 			ourUnmount = true
// 		}
// 	} else if snapVol.contentType == ContentTypeFS && linux.IsMountPoint(mountPath) {
// 		if refCount > 0 {
// 			d.logger.Debug("Skipping unmount as in use", logger.Ctx{"volName": snapVol.name, "refCount": refCount})
// 			return false, ErrInUse
// 		}

// 		_, err := forceUnmount(mountPath)
// 		if err != nil {
// 			return false, err
// 		}

// 		d.logger.Debug("Unmounted ZFS snapshot dataset", logger.Ctx{"dev": snapshotDataset, "path": mountPath})
// 		ourUnmount = true
// 	}

// 	return ourUnmount, nil
// }

// VolumeSnapshots returns a list of snapshots for the volume (in no particular order).
func (d *truenas) VolumeSnapshots(vol Volume, op *operations.Operation) ([]string, error) {
	// Get all children datasets.
	entries, err := d.getDatasets(d.dataset(vol, false), "snapshot")
	if err != nil {
		return nil, err
	}

	// Filter only the snapshots.
	snapshots := []string{}
	for _, entry := range entries {
		if strings.HasPrefix(entry, "@snapshot-") {
			snapshots = append(snapshots, strings.TrimPrefix(entry, "@snapshot-"))
		}
	}

	return snapshots, nil
}

// RestoreVolume restores a volume from a snapshot.
func (d *truenas) RestoreVolume(vol Volume, snapshotName string, op *operations.Operation) error {
	return d.restoreVolume(vol, snapshotName, false, op)
}

func (d *truenas) restoreVolume(vol Volume, snapshotName string, migration bool, op *operations.Operation) error {
	// Get the list of snapshots.
	entries, err := d.getDatasets(d.dataset(vol, false), "snapshot")
	if err != nil {
		return err
	}

	// Check if more recent snapshots exist.
	idx := -1
	snapshots := []string{}
	for i, entry := range entries {
		if entry == fmt.Sprintf("@snapshot-%s", snapshotName) {
			// Located the current snapshot.
			idx = i
			continue
		} else if idx < 0 {
			// Skip any previous snapshot.
			continue
		}

		if strings.HasPrefix(entry, "@snapshot-") {
			// Located a normal snapshot following ours.
			snapshots = append(snapshots, strings.TrimPrefix(entry, "@snapshot-"))
			continue
		}

		if strings.HasPrefix(entry, "@") {
			// Located an internal snapshot.
			return fmt.Errorf("Snapshot %q cannot be restored due to subsequent internal snapshot(s) (from a copy)", snapshotName)
		}
	}

	// Check if snapshot removal is allowed.
	if len(snapshots) > 0 {
		if util.IsFalseOrEmpty(vol.ExpandedConfig("zfs.remove_snapshots")) {
			return fmt.Errorf("Snapshot %q cannot be restored due to subsequent snapshot(s). Set zfs.remove_snapshots to override", snapshotName)
		}

		// Setup custom error to tell the backend what to delete.
		err := ErrDeleteSnapshots{}
		err.Snapshots = snapshots
		return err
	}

	// Restore the snapshot.
	datasets, err := d.getDatasets(d.dataset(vol, false), "snapshot")
	if err != nil {
		return err
	}

	toRollback := make([]string, 0)
	for _, dataset := range datasets {
		if !strings.HasSuffix(dataset, fmt.Sprintf("@snapshot-%s", snapshotName)) {
			continue
		}

		toRollback = append(toRollback, fmt.Sprintf("%s%s", d.dataset(vol, false), dataset))
	}

	if len(toRollback) > 0 {
		snapRbCmd := []string{"snapshot", "rollback"}
		_, err = d.runTool(append(snapRbCmd, toRollback...)...)
		if err != nil {
			return err
		}
	}

	if vol.contentType == ContentTypeFS && d.isBlockBacked(vol) && renegerateFilesystemUUIDNeeded(vol.ConfigBlockFilesystem()) {
		// _, err = d.activateVolume(vol)
		// if err != nil {
		// 	return err
		// }

		//defer func() { _, _ = d.deactivateVolume(vol) }()

		volPath, err := d.GetVolumeDiskPath(vol)
		if err != nil {
			return err
		}

		d.logger.Debug("Regenerating filesystem UUID", logger.Ctx{"dev": volPath, "fs": vol.ConfigBlockFilesystem()})
		err = regenerateFilesystemUUID(vol.ConfigBlockFilesystem(), volPath)
		if err != nil {
			return err
		}
	}

	// For VM images, restore the associated filesystem dataset too.
	if !migration && vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.restoreVolume(fsVol, snapshotName, migration, op)
		if err != nil {
			return err
		}
	}

	return nil
}

// RenameVolumeSnapshot renames a volume snapshot.
func (d *truenas) RenameVolumeSnapshot(vol Volume, newSnapshotName string, op *operations.Operation) error {
	parentName, _, _ := api.GetParentAndSnapshotName(vol.name)
	newVol := NewVolume(d, d.name, vol.volType, vol.contentType, fmt.Sprintf("%s/%s", parentName, newSnapshotName), vol.config, vol.poolConfig)

	// Revert handling.
	revert := revert.New()
	defer revert.Fail()

	// First rename the VFS paths.
	err := genericVFSRenameVolumeSnapshot(d, vol, newSnapshotName, op)
	if err != nil {
		return err
	}

	revert.Add(func() {
		_ = genericVFSRenameVolumeSnapshot(d, newVol, vol.name, op)
	})

	// Rename the ZFS datasets.
	//_, err = subprocess.RunCommand("zfs", "rename", d.dataset(vol, false), d.dataset(newVol, false))
	out, err := d.renameSnapshot(d.dataset(vol, false), d.dataset(newVol, false))

	_ = out
	if err != nil {
		return err
	}

	revert.Add(func() {
		//_, _ = subprocess.RunCommand("zfs", "rename", d.dataset(newVol, false), d.dataset(vol, false))
		_, _ = d.renameSnapshot(d.dataset(newVol, false), d.dataset(vol, false))

	})

	// All done.
	revert.Success()

	return nil
}

// FillVolumeConfig populate volume with default config.
func (d *truenas) FillVolumeConfig(vol Volume) error {

	var excludedKeys []string

	// Copy volume.* configuration options from pool.
	// If vol has a source, ignore the block mode related config keys from the pool.
	if vol.hasSource || vol.IsVMBlock() || vol.volType == VolumeTypeCustom && vol.contentType == ContentTypeBlock {
		excludedKeys = []string{"truenas.block_mode", "block.filesystem", "block.mount_options"}
	} else if vol.volType == VolumeTypeCustom && !vol.IsBlockBacked() {
		excludedKeys = []string{"block.filesystem", "block.mount_options"}
	}

	// Copy volume.* configuration options from pool.
	// Exclude 'block.filesystem' and 'block.mount_options'
	// as this ones are handled below in this function and depends from volume type
	err := d.fillVolumeConfig(&vol, excludedKeys...)
	if err != nil {
		return err
	}

	// Only validate filesystem config keys for filesystem volumes or VM block volumes (which have an
	// associated filesystem volume).

	if vol.ContentType() == ContentTypeFS {
		//we default block_mode to true...
		if vol.config["truenas.block_mode"] == "" {
			//vol.config["truenas.block_mode"] = "true"
		}
	}

	if vol.ContentType() == ContentTypeFS /*|| vol.IsVMBlock()*/ {
		// Inherit filesystem from pool if not set.
		if vol.config["block.filesystem"] == "" {
			vol.config["block.filesystem"] = d.config["volume.block.filesystem"]
		}

		// Default filesystem if neither volume nor pool specify an override.
		if vol.config["block.filesystem"] == "" {
			// Unchangeable volume property: Set unconditionally.
			vol.config["block.filesystem"] = DefaultFilesystem
		}

		// Inherit filesystem mount options from pool if not set.
		if vol.config["block.mount_options"] == "" {
			vol.config["block.mount_options"] = d.config["volume.block.mount_options"]
		}

		// Default filesystem mount options if neither volume nor pool specify an override.
		if vol.config["block.mount_options"] == "" {
			// Unchangeable volume property: Set unconditionally.
			vol.config["block.mount_options"] = "discard"
		}
	}

	return nil
}

func (d *truenas) isBlockBacked(vol Volume) bool {
	//return util.IsTrue(vol.Config()["truenas.block_mode"])
	return vol.contentType == ContentTypeFS && vol.config["block.filesystem"] != ""
}
