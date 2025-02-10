package drivers

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lxc/incus/v6/internal/migration"
	deviceConfig "github.com/lxc/incus/v6/internal/server/device/config"
	localMigration "github.com/lxc/incus/v6/internal/server/migration"
	"github.com/lxc/incus/v6/internal/server/operations"
	"github.com/lxc/incus/v6/internal/version"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/logger"
	"github.com/lxc/incus/v6/shared/revert"
	"github.com/lxc/incus/v6/shared/subprocess"
	"github.com/lxc/incus/v6/shared/units"
	"github.com/lxc/incus/v6/shared/util"
)

var tnVersion string
var tnLoaded bool

// TODO: these flags are not needed once we stop using earlier versions.
var tnHasLoginFlags bool          // 0.1.1
var tnHasShareNfs bool            // 0.1.2
var tnHasUpdateShares bool        // 0.1.4
var tnHasNfsDeleteByDataset bool  // 0.1.6
var tnHasNfsUpdateWithCreate bool // 0.1.7
var tnHasErrorResults bool        // 0.1.8

var tnDefaultSettings = map[string]string{
	"atime": "off", //"relatime": "on",
	//"mountpoint": "legacy",
	//"setuid":  "on",
	"exec": "on",
	//"devices": "on",
	"acltype": "posix", //"acltype": "posixacl",
	"aclmode": "discard",
	//"xattr":      "sa", 			// xattr doesn't seem able to be set via API
	//"share_type": "nfs",
	"comments":  "Managed by Incus.TrueNAS", // these are set in createDataset
	"managedby": "incus.truenas",
}

type truenas struct {
	common
}

func (d *truenas) isVersionGE(thisVersion version.DottedVersion, thatVersion string) bool {
	ver, err := version.Parse(thatVersion)
	if err != nil {
		return false
	}

	return (thisVersion.Compare(ver) >= 0)
}

func (d *truenas) initVersionAndCapabilities() error {
	// Get the version information.
	if tnVersion == "" {
		version, err := d.version()
		if err != nil {
			return err
		}

		tnVersion = version
	}

	ourVer, err := version.Parse(tnVersion)
	if err != nil {
		return err
	}

	// Decide whether we can use features added by a specific version
	tnHasLoginFlags = d.isVersionGE(*ourVer, "0.1.1")          // login flags (api-key, url, key-file)
	tnHasShareNfs = d.isVersionGE(*ourVer, "0.1.2")            // create/list/delete NFS shares
	tnHasUpdateShares = d.isVersionGE(*ourVer, "0.1.4")        // can update-shares when renaming datasets
	tnHasNfsDeleteByDataset = d.isVersionGE(*ourVer, "0.1.6")  // can delete shares by dataset
	tnHasNfsUpdateWithCreate = d.isVersionGE(*ourVer, "0.1.7") // can create shares with update
	tnHasErrorResults = d.isVersionGE(*ourVer, "0.1.8")        // actually returns errors on failure

	return nil
}

// load is used to run one-time action per-driver rather than per-pool.
func (d *truenas) load() error {
	// Register the patches.
	d.patches = map[string]func() error{
		"storage_lvm_skipactivation":                         nil,
		"storage_missing_snapshot_records":                   nil,
		"storage_delete_old_snapshot_records":                nil,
		"storage_zfs_drop_block_volume_filesystem_extension": nil, //d.patchDropBlockVolumeFilesystemExtension,
		"storage_prefix_bucket_names_with_project":           nil,
	}

	// Done if previously loaded.
	if tnLoaded {
		return nil
	}

	// Validate the needed tools are present.
	for _, tool := range []string{tnToolName} {
		_, err := exec.LookPath(tool)
		if err != nil {
			return fmt.Errorf("Required tool '%s' is missing", tool)
		}
	}

	// also tests for available features
	err := d.initVersionAndCapabilities()
	if err != nil {
		return err
	}

	tnLoaded = true

	return nil
}

// Info returns info about the driver and its environment.
func (d *truenas) Info() Info {
	info := Info{
		Name:                         "truenas",
		Version:                      tnVersion,
		DefaultVMBlockFilesystemSize: deviceConfig.DefaultVMBlockFilesystemSize,
		OptimizedImages:              true,
		OptimizedBackups:             true,
		PreservesInodes:              true,
		Remote:                       d.isRemote(),
		VolumeTypes:                  []VolumeType{VolumeTypeCustom, VolumeTypeImage, VolumeTypeContainer, VolumeTypeVM},
		VolumeMultiNode:              d.isRemote(),
		BlockBacking:                 false,
		RunningCopyFreeze:            false,
		DirectIO:                     false,
		MountedRoot:                  false,
		Buckets:                      false,
	}

	return info
}

// ensureInitialDatasets creates missing initial datasets or configures existing ones with current policy.
// Accepts warnOnExistingPolicyApplyError argument, if true will warn rather than fail if applying current policy
// to an existing dataset fails.
func (d truenas) ensureInitialDatasets(warnOnExistingPolicyApplyError bool) error {
	args := make([]string, 0, len(tnDefaultSettings))
	for k, v := range tnDefaultSettings {
		args = append(args, fmt.Sprintf("%s=%s", k, v))
	}

	if d.config["truenas.dataset"] == "" {
		return nil
	}

	err := d.setDatasetProperties(d.config["truenas.dataset"], args...)
	if err != nil {
		if warnOnExistingPolicyApplyError {
			d.logger.Warn("Failed applying policy to existing dataset", logger.Ctx{"dataset": d.config["truenas.dataset"], "err": err})
		} else {
			return fmt.Errorf("Failed applying policy to existing dataset %q: %w", d.config["truenas.dataset"], err)
		}
	}

	for _, dataset := range d.initialDatasets() {
		//properties := []string{"mountpoint=legacy"}
		properties := []string{}
		// if slices.Contains([]string{"virtual-machines", "deleted/virtual-machines"}, dataset) {
		// 	properties = append(properties, "volmode=none")
		// }

		datasetPath := filepath.Join(d.config["truenas.dataset"], dataset)
		exists, err := d.datasetExists(datasetPath)
		if err != nil {
			return err
		}

		if exists {
			err = d.setDatasetProperties(datasetPath, properties...)
			if err != nil {
				if warnOnExistingPolicyApplyError {
					d.logger.Warn("Failed applying policy to existing dataset", logger.Ctx{"dataset": datasetPath, "err": err})
				} else {
					return fmt.Errorf("Failed applying policy to existing dataset %q: %w", datasetPath, err)
				}
			}
		} else {
			err = d.createDataset(datasetPath, properties...)
			if err != nil {
				return fmt.Errorf("Failed creating dataset %q: %w", datasetPath, err)
			}
		}
	}

	return nil
}

// FillConfig populates the storage pool's configuration file with the default values.
func (d *truenas) FillConfig() error {

	// map back. for legacy.
	if d.config["truenas.dataset"] != "" && d.config["source"] == "" {
		d.config["source"] = d.config["truenas.dataset"]
	}

	// set host url
	if d.config["truenas.url"] == "" && d.config["truenas.host"] != "" {
		d.config["truenas.url"] = fmt.Sprintf("wss://%s/api/current", d.config["truenas.host"])
	}

	return nil
}

// Create is called during pool creation and is effectively using an empty driver struct.
// WARNING: The Create() function cannot rely on any of the struct attributes being set.
func (d *truenas) Create() error {

	// Store the provided source as we are likely to be mangling it.
	d.config["volatile.initial_source"] = d.config["source"]

	err := d.FillConfig()

	if d.config["source"] == "" || filepath.IsAbs(d.config["source"]) {
		return fmt.Errorf(`TrueNAS Driver requires "source" to be specified using the format: <remote pool>[[/<remote dataset>]...][/]`)
	}

	// a pool... means we create a dataset in the root
	if !strings.Contains(d.config["source"], "/") {
		d.config["source"] += "/"
	}

	// a trailing slash means use the storage pool name as the dataset
	if strings.HasSuffix(d.config["source"], "/") {
		d.config["source"] += d.name
	}

	// legacy stores source in truenas.dataset
	d.config["truenas.dataset"] = d.config["source"]

	// create pool dataset
	exists, err := d.datasetExists(d.config["truenas.dataset"])
	if err != nil {
		return err
	}

	if !exists {
		err = d.createDataset(d.config["source"])
		if err != nil {
			return fmt.Errorf("Failed to create storgage pool on TrueNAS host: %s", d.config["source"])
		}
	} else {
		// Confirm that the existing pool/dataset is all empty.
		datasets, err := d.getDatasets(d.config["truenas.dataset"], "all")
		if err != nil {
			return err
		}

		if len(datasets) > 0 {
			return fmt.Errorf(`Remote TrueNAS dataset isn't empty: %s`, d.config["truenas.dataset"])
		}

	}

	// Setup revert in case of problems
	revert := revert.New()
	defer revert.Fail()

	revert.Add(func() { _ = d.Delete(nil) })

	// Apply our default configuration.
	err = d.ensureInitialDatasets(false)
	if err != nil {
		return err
	}

	revert.Success()
	return nil
}

// Delete removes the storage pool from the storage device.
func (d *truenas) Delete(op *operations.Operation) error {
	// Check if the dataset/pool is already gone.
	exists, err := d.datasetExists(d.config["truenas.dataset"])
	if err != nil {
		return err
	}

	if exists {
		// Confirm that nothing's been left behind
		datasets, err := d.getDatasets(d.config["truenas.dataset"], "all")
		if err != nil {
			return err
		}

		initialDatasets := d.initialDatasets()
		for _, dataset := range datasets {
			dataset = strings.TrimPrefix(dataset, "/")

			if slices.Contains(initialDatasets, dataset) {
				continue
			}

			fields := strings.Split(dataset, "/")
			if len(fields) > 1 {
				return fmt.Errorf("TrueNAS pool has leftover datasets: %s", dataset)
			}
		}

		// Delete the dataset.
		out, err := d.runTool("dataset", "delete", "-r", d.config["truenas.dataset"])
		_ = out
		if err != nil {
			return err
		}
	}

	// On delete, wipe everything in the directory.
	err = wipeDirectory(GetPoolMountPath(d.name))
	if err != nil {
		return err
	}

	return nil
}

// Validate checks that all provide keys are supported and that no conflicting or missing configuration is present.
func (d *truenas) Validate(config map[string]string) error {
	// rules := map[string]func(value string) error{
	// 	"size":          validate.Optional(validate.IsSize),
	// 	"zfs.pool_name": validate.IsAny,
	// 	"zfs.clone_copy": validate.Optional(func(value string) error {
	// 		if value == "rebase" {
	// 			return nil
	// 		}

	// 		return validate.IsBool(value)
	// 	}),
	// 	"zfs.export": validate.Optional(validate.IsBool),
	// }

	return nil //d.validatePool(config, rules, d.commonVolumeRules())
}

// Update applies any driver changes required from a configuration change.
func (d *truenas) Update(changedConfig map[string]string) error {
	// _, ok := changedConfig["zfs.pool_name"]
	// if ok {
	// 	return fmt.Errorf("zfs.pool_name cannot be modified")
	// }

	// size, ok := changedConfig["size"]
	// if ok {
	// 	// Figure out loop path
	// 	loopPath := loopFilePath(d.name)

	// 	_, devices := d.parseSource()
	// 	if len(devices) != 1 || devices[0] != loopPath {
	// 		return fmt.Errorf("Cannot resize non-loopback pools")
	// 	}

	// 	// Resize loop file
	// 	f, err := os.OpenFile(loopPath, os.O_RDWR, 0600)
	// 	if err != nil {
	// 		return err
	// 	}

	// 	defer func() { _ = f.Close() }()

	// 	sizeBytes, _ := units.ParseByteSizeString(size)

	// 	err = f.Truncate(sizeBytes)
	// 	if err != nil {
	// 		return err
	// 	}

	// 	_, err = subprocess.RunCommand("zpool", "online", "-e", d.config["zfs.pool_name"], loopPath)
	// 	if err != nil {
	// 		return err
	// 	}
	// }

	return nil
}

// Mount mounts the storage pool.
func (d *truenas) Mount() (bool, error) {
	// // Import the pool if not already imported.
	// imported, err := d.importPool()
	// if err != nil {
	// 	return false, err
	// }

	// Apply our default configuration.
	err := d.ensureInitialDatasets(true)
	if err != nil {
		return false, err
	}

	//return imported, nil
	return false, nil
}

// Unmount unmounts the storage pool.
func (d *truenas) Unmount() (bool, error) {
	// // Skip if zfs.export config is set to false
	// if util.IsFalse(d.config["zfs.export"]) {
	// 	return false, nil
	// }

	// // Skip if using a dataset and not a full pool.
	// if strings.Contains(d.config["zfs.pool_name"], "/") {
	// 	return false, nil
	// }

	// // Check if already unmounted.
	// exists, err := d.datasetExists(d.config["zfs.pool_name"])
	// if err != nil {
	// 	return false, err
	// }

	// if !exists {
	// 	return false, nil
	// }

	// // Export the pool.
	// poolName := strings.Split(d.config["zfs.pool_name"], "/")[0]
	// _, err = subprocess.RunCommand("zpool", "export", poolName)
	// if err != nil {
	// 	return false, err
	// }

	return true, nil
}

func (d *truenas) GetResources() (*api.ResourcesStoragePool, error) {
	// // Get the total amount of space.
	// availableStr, err := d.getDatasetProperty(d.config["zfs.pool_name"], "available")
	// if err != nil {
	// 	return nil, err
	// }

	// available, err := strconv.ParseUint(strings.TrimSpace(availableStr), 10, 64)
	// if err != nil {
	// 	return nil, err
	// }

	// // Get the used amount of space.
	// usedStr, err := d.getDatasetProperty(d.config["zfs.pool_name"], "used")
	// if err != nil {
	// 	return nil, err
	// }

	// used, err := strconv.ParseUint(strings.TrimSpace(usedStr), 10, 64)
	// if err != nil {
	// 	return nil, err
	// }

	// // Build the struct.
	// // Inode allocation is dynamic so no use in reporting them.
	// res := api.ResourcesStoragePool{}
	// res.Space.Total = used + available
	// res.Space.Used = used

	//return &res, nil
	return nil, nil
}

// MigrationType returns the type of transfer methods to be used when doing migrations between pools in preference order.
func (d *truenas) MigrationTypes(contentType ContentType, refresh bool, copySnapshots bool) []localMigration.Type {
	var rsyncFeatures []string

	// Do not pass compression argument to rsync if the associated
	// config key, that is rsync.compression, is set to false.
	if util.IsFalse(d.Config()["rsync.compression"]) {
		rsyncFeatures = []string{"xattrs", "delete", "bidirectional"}
	} else {
		rsyncFeatures = []string{"xattrs", "delete", "compress", "bidirectional"}
	}

	// Detect ZFS features.
	features := []string{migration.ZFSFeatureMigrationHeader, "compress"}

	if contentType == ContentTypeFS {
		features = append(features, migration.ZFSFeatureZvolFilesystems)
	}

	if IsContentBlock(contentType) {
		return []localMigration.Type{
			{
				FSType:   migration.MigrationFSType_ZFS,
				Features: features,
			},
			{
				FSType:   migration.MigrationFSType_BLOCK_AND_RSYNC,
				Features: rsyncFeatures,
			},
		}
	}

	if refresh && !copySnapshots {
		return []localMigration.Type{
			{
				FSType:   migration.MigrationFSType_RSYNC,
				Features: rsyncFeatures,
			},
		}
	}

	return []localMigration.Type{
		{
			FSType:   migration.MigrationFSType_ZFS,
			Features: features,
		},
		{
			FSType:   migration.MigrationFSType_RSYNC,
			Features: rsyncFeatures,
		},
	}
}

// patchDropBlockVolumeFilesystemExtension removes the filesystem extension (e.g _ext4) from VM image block volumes.
func (d *truenas) patchDropBlockVolumeFilesystemExtension() error {
	poolName, ok := d.config["zfs.pool_name"]
	if !ok {
		poolName = d.name
	}

	out, err := subprocess.RunCommand("zfs", "list", "-H", "-r", "-o", "name", "-t", "volume", fmt.Sprintf("%s/images", poolName))
	if err != nil {
		return fmt.Errorf("Failed listing images: %w", err)
	}

	for _, volume := range strings.Split(out, "\n") {
		fields := strings.SplitN(volume, fmt.Sprintf("%s/images/", poolName), 2)

		if len(fields) != 2 || fields[1] == "" {
			continue
		}

		// Ignore non-block images, and images without filesystem extension
		if !strings.HasSuffix(fields[1], ".block") || !strings.Contains(fields[1], "_") {
			continue
		}

		// Rename zfs dataset. Snapshots will automatically be renamed.
		newName := fmt.Sprintf("%s/images/%s.block", poolName, strings.Split(fields[1], "_")[0])

		_, err = subprocess.RunCommand("zfs", "rename", volume, newName)
		if err != nil {
			return fmt.Errorf("Failed renaming zfs dataset: %w", err)
		}
	}

	return nil
}

// roundVolumeBlockSizeBytes returns sizeBytes rounded up to the next multiple
// of `vol`'s "zfs.blocksize".
func (d *truenas) roundVolumeBlockSizeBytes(vol Volume, sizeBytes int64) (int64, error) {
	minBlockSize, err := units.ParseByteSizeString(vol.ExpandedConfig("zfs.blocksize"))

	// minBlockSize will be 0 if zfs.blocksize=""
	if minBlockSize <= 0 || err != nil {
		// 16KiB is the default volblocksize
		minBlockSize = 16 * 1024
	}

	return roundAbove(minBlockSize, sizeBytes), nil
}
