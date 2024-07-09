package tunelinux

// Copyright 2019 Hewlett Packard Enterprise Development LP.
import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/hpe-storage/common-host-libs/linux"
	log "github.com/hpe-storage/common-host-libs/logger"
	"github.com/hpe-storage/common-host-libs/model"
	"github.com/hpe-storage/common-host-libs/mpathconfig"
	"github.com/hpe-storage/common-host-libs/util"
)

const (
	multipath = "multipath"

	// multipath params
	multipathParamPattern = "\\s*(?P<name>.*?)\\s+(?P<value>.*)"
)

var (
	deviceBlockPattern = map[string]string{
		"Nimble": "(?s)devices\\s+{\\s*.*device\\s*{(?P<device_block>.*Nimble.*?)}",
		"3par":   "(?s)devices\\s+{\\s*.*device\\s*{(?P<device_block>.*3PAR.*?)}",
	}
	mountMutex              sync.Mutex
	umountMutex             sync.Mutex
	staleDeviceRemovalMutex sync.Mutex
)

// GetMultipathConfigFile returns path of the template multipath.conf file according to OS distro
func GetMultipathTemplateFile() (configFile string, err error) {
	log.Traceln(">>>>> GetMultipathTemplateFile")
	defer log.Traceln("<<<<< GetMultipathTemplateFile")

	// assume current directory by default
	configPath := "./"
	// assume generic config by default
	multipathConfig := "multipath.conf.generic"

	// get config base path
	if util.GetNltHome() != "" {
		// path bundled with NLT
		configPath = util.GetNltHome() + "nimbletune/"
	}

	// get os distro to determine approprite multipath settings
	osInfo, err := linux.GetOsInfo()
	if err != nil {
		return "", err
	}
	major, err := strconv.Atoi(osInfo.GetOsMajorVersion())
	if err != nil {
		return "", err
	}

	switch osInfo.GetOsDistro() {
	// Ubuntu 18 settings are latest
	case linux.OsTypeUbuntu:
		if major >= 18 {
			multipathConfig = "multipath.conf.upstream"
		}
	// RHEL/CentOS 8 settings are latest
	case linux.OsTypeRedhat:
		fallthrough
	case linux.OsTypeCentos:
		if major >= 8 {
			multipathConfig = "multipath.conf.upstream"
		}
	}

	log.Tracef("using multipath template file as %s", configPath+multipathConfig)
	return configPath + multipathConfig, nil
}

// getMultipathDeviceParamRecommendation returns recommendation for given param of device section in multipath.conf
func getMultipathDeviceParamRecommendation(paramKey string, currentValue string, recommendedValue string, description string, severity string) (recommendation *Recommendation, err error) {
	log.Trace("getMultipathDeviceParamRecommendation called with paramKey ", paramKey, " value ", currentValue, " recommended ", recommendedValue)
	var optionSetting *Recommendation

	// create recommendation
	if currentValue == recommendedValue || strings.Replace(currentValue, "\"", "", 2) == recommendedValue {
		optionSetting = &Recommendation{
			CompliantStatus: ComplianceStatus.String(Recommended),
		}
	} else {
		optionSetting = &Recommendation{
			CompliantStatus: ComplianceStatus.String(NotRecommended),
		}
	}
	// set common attributes
	optionSetting.ID = linux.HashMountID("multipath" + "device" + paramKey)
	optionSetting.Category = Category.String(Multipath)
	optionSetting.Level = severity
	optionSetting.Description = description
	optionSetting.Parameter = paramKey
	optionSetting.Value = currentValue
	optionSetting.Recommendation = recommendedValue
	optionSetting.Device = All
	optionSetting.FsType = ""
	optionSetting.Vendor = ""

	return optionSetting, nil
}

// getMultipathDeviceScopeRecommendations obtain recommendations for block section of multipath.conf
func getMultipathDeviceScopeRecommendations(deviceBlock string) (settings []DeviceRecommendation, err error) {
	log.Trace("getMultipathDeviceScopeRecommendations called")
	var recommendation *Recommendation
	var deviceRecommendations []DeviceRecommendation
	var keyFound bool
	var paramValue string
	var paramKey string

	deviceBlockRecommendationMap, _ := getParamToTemplateFieldMap(Multipath, "recommendation", "")
	deviceSettingsDescriptionMap, _ := getParamToTemplateFieldMap(Multipath, "description", "")
	deviceSettingsSeverityMap, _ := getParamToTemplateFieldMap(Multipath, "severity", "")

	// get individual parameters from device block
	currentSettings := strings.Split(string(deviceBlock), "\n")

	for index, _ := range deviceBlockRecommendationMap {

		var currRecommendation DeviceRecommendation
		for key := range deviceBlockRecommendationMap[index].deviceMap {

			keyFound = false
			for _, setting := range currentSettings {
				if setting != "" {
					r := regexp.MustCompile(multipathParamPattern)
					// extract key value from parameter string
					if r.MatchString(setting) {
						result := util.FindStringSubmatchMap(setting, r)
						paramKey = result["name"]
						paramValue = result["value"]
					} else {
						log.Error("Invalid multipath device param value for recommendation ", setting)
						continue
					}
					if paramKey == key {
						// found the matching key for recommended parameter in /etc/multipath.conf
						keyFound = true
						break
					}
				}
			}
			var description = deviceSettingsDescriptionMap[index].deviceMap[key]
			var recommendedValue = deviceBlockRecommendationMap[index].deviceMap[key]
			var severity = deviceSettingsSeverityMap[index].deviceMap[key]
			if keyFound == true {
				log.Info(" Keyfound = ", keyFound)
				// entry found in /etc/multipath.conf
				recommendation, err = getMultipathDeviceParamRecommendation(paramKey, strings.TrimSpace(paramValue), recommendedValue, description, severity)
				if err != nil {
					log.Error("Unable to get recommendation for multipath param", paramKey, "value ", paramValue)
					continue
				}
			} else {
				// missing needed parameters in /etc/multipath.conf
				recommendation, err = getMultipathDeviceParamRecommendation(key, "", recommendedValue, description, severity)
				if err != nil {
					log.Error("Unable to get recommendation for multipath param", paramKey, "value ", paramValue)
					continue
				}
			}
			currRecommendation.RecomendArray = append(currRecommendation.RecomendArray, recommendation)
		}
		currRecommendation.DeviceType = deviceBlockRecommendationMap[index].DeviceType
		deviceRecommendations = append(deviceRecommendations, currRecommendation)
	}
	return deviceRecommendations, nil
}

// IsMultipathRequired returns if multipath needs to be enabled on the system
func IsMultipathRequired() (required bool, err error) {
	var isVM bool
	var multipathRequired = true
	// identify if running as a virtual machine and guest iscsi is enabled
	isVM, err = linux.IsVirtualMachine()
	if err != nil {
		log.Error("unable to determine if system is running as a virtual machine ", err.Error())
		return false, err
	}
	if isVM && !IsIscsiEnabled() {
		log.Error("system is running as a virtual machine and guest iSCSI is not enabled. Skipping multipath recommendations")
		multipathRequired = false
	}
	return multipathRequired, nil
}

// GetMultipathRecommendations obtain various recommendations for multipath settings on host
func GetMultipathRecommendations() (settings []DeviceRecommendation, err error) {
	log.Trace("GetMultipathRecommendations called")
	var deviceRecommendations []DeviceRecommendation

	var isMultipathRequired bool

	// check if multipath is required in the first place on the system
	isMultipathRequired, err = IsMultipathRequired()
	if err != nil {
		log.Error("unable to determine if multipath is required ", err.Error())
		return nil, err
	}
	if !isMultipathRequired {
		log.Info("multipath is not required on the system, skipping get multipath recommendations")
		return nil, nil
	}
	// load config settings
	err = loadTemplateSettings()
	if err != nil {
		return nil, err
	}

	for _, devicePattern := range deviceBlockPattern {
		// Check if /etc/multipath.conf present
		if _, err = os.Stat(linux.MultipathConf); os.IsNotExist(err) {
			log.Error("/etc/multipath.conf file missing")
			// Generate All Recommendations By default
			deviceRecommendations, err = getMultipathDeviceScopeRecommendations("")
			if err != nil {
				log.Error("Unable to get recommendations for multipath device settings ", err.Error())
			}
			return deviceRecommendations, err
		}
		// Obtain contents of /etc/multipath.conf
		content, err := ioutil.ReadFile(linux.MultipathConf)
		if err != nil {
			log.Error(err.Error())
			return nil, err
		}

		r := regexp.MustCompile(devicePattern)
		if r.MatchString(string(content)) {
			// found Device block
			result := util.FindStringSubmatchMap(string(content), r)
			deviceBlock := result["device_block"]
			deviceRecommendations, err = getMultipathDeviceScopeRecommendations(strings.TrimSpace(deviceBlock))
			if err != nil {
				log.Error("Unable to get recommendations for multipath device settings ", err.Error())
			}
		} else {
			// Device section missing.
			// Generate All Recommendations By default
			deviceRecommendations, err = getMultipathDeviceScopeRecommendations("")
			if err != nil {
				log.Error("Unable to get recommendations for multipath device settings ", err.Error())
			}
		}
	}

	return deviceRecommendations, err
}

// setMultipathRecommendations sets device scope recommendations in multipath.conf
func setMultipathRecommendations(recommendations []*Recommendation, device string) (err error) {
	var devicesSection *mpathconfig.Section
	var deviceSection *mpathconfig.Section
	var defaultsSection *mpathconfig.Section
	// parse multipath.conf into different sections and apply recommendation
	config, err := mpathconfig.ParseConfig(linux.MultipathConf)
	if err != nil {
		return err
	}

	deviceSection, err = config.GetDeviceSection(device)
	if err != nil {
		devicesSection, err = config.GetSection("devices", "")
		if err != nil {
			// Device section is not found, get or create devices{} and then add device{} section
			devicesSection, err = config.AddSection("devices", config.GetRoot())
			if err != nil {
				return errors.New("Unable to add new devices section")
			}
		}
		deviceSection, err = config.AddSection("device", devicesSection)
		if err != nil {
			return errors.New("Unable to add new nimble device section")
		}
	}
	// update recommended values in device section
	for _, recommendation := range recommendations {
		deviceSection.GetProperties()[recommendation.Parameter] = recommendation.Recommendation
	}

	// update find_multipaths as no if set in defaults section
	defaultsSection, err = config.GetSection("defaults", "")
	if err != nil {
		// add a defaults section with override for find_multipaths
		defaultsSection, err = config.AddSection("defaults", config.GetRoot())
		if err != nil {
			return errors.New("Unable to add new defaults section in /etc/multipath.conf")
		}
	}
	if err == nil {
		// if we find_multipaths key with yes value or if the key is absent (in case of Ubuntu)
		// set it to no
		value := (defaultsSection.GetProperties())["find_multipaths"]
		if value == "yes" || value == "" {
			(defaultsSection.GetProperties())["find_multipaths"] = "no"
		}
	}

	// save modified configuration
	err = mpathconfig.SaveConfig(config, linux.MultipathConf)
	if err != nil {
		return err
	}

	return nil
}

// SetMultipathRecommendations sets multipath.conf settings
func SetMultipathRecommendations() (err error) {
	log.Traceln(">>>>> SetMultipathRecommendations")
	defer log.Traceln("<<<<< SetMultipathRecommendations")

	// Take a backup of existing multipath.conf
	f, err := os.Stat(linux.MultipathConf)

	if err != nil || f.Size() == 0 {
		multipathTemplate, err := GetMultipathTemplateFile()
		if err != nil {
			return err
		}
		// Copy the multipath.conf supplied with utility
		err = util.CopyFile(multipathTemplate, linux.MultipathConf)
		if err != nil {
			return err
		}
	}
	// Get current recommendations
	recommendations, err := GetMultipathRecommendations()
	if err != nil {
		return err
	}
	if len(recommendations) == 0 {
		log.Warning("no recommendations found for multipath.conf settings")
		return nil
	}

	// Apply new recommendations for mismatched values
	for _, dev := range recommendations {
		err = setMultipathRecommendations(dev.RecomendArray, dev.DeviceType)
	}
	if err != nil {
		return err
	}
	// Start service as it would have failed to start initially if multipath.conf is missing
	err = linux.ServiceCommand(multipath, "start")
	if err != nil {
		return err
	}

	// Reconfigure settings in any case to make sure new settings are applied
	_, err = linux.MultipathdReconfigure()
	if err != nil {
		return err
	}
	log.Info("Successfully configured multipath.conf settings")
	return nil
}

// ConfigureMultipath ensures following
// 1. Service is enabled and running
// 2. Multipath settings are configured correctly
func ConfigureMultipath() (err error) {
	log.Traceln(">>>>> ConfigureMultipath")
	defer log.Traceln("<<<<< ConfigureMultipath")

	// Ensure multipath.conf settings
	err = SetMultipathRecommendations()
	if err != nil {
		return err
	}
	return nil
}

func GetMultipathDevices() (multipathDevices []model.MultipathDevice, err error) {
	log.Tracef(">>>> getMultipathDevices ")
	defer log.Trace("<<<<< getMultipathDevices")

	out, _, err := util.ExecCommandOutput("multipathd", []string{"show", "multipaths", "json"})

	if err != nil {
		return nil, fmt.Errorf("Failed to get the multipath devices due to the error: %s", err.Error())
	}

	if out != "" {
		multipathJson := new(model.MultipathInfo)
		err = json.Unmarshal([]byte(out), multipathJson)
		if err != nil {
			return nil, fmt.Errorf("Invalid JSON output of multipathd command: %s", err.Error())
		}

		for _, mapItem := range multipathJson.Maps {
			if len(mapItem.Vend) > 0 && isSupportedDeviceVendor(linux.DeviceVendorPatterns, mapItem.Vend) {
				if mapItem.Paths < 1 && mapItem.PathFaults > 0 {
					mapItem.IsUnhealthy = true
				}
				multipathDevices = append(multipathDevices, mapItem)
				log.Tracef("Multipath device: %s", mapItem.Name)
			}
		}
		log.Infof("Found %d multipath devices %+v", len(multipathDevices), multipathDevices)
		return multipathDevices, nil
	}
	return nil, fmt.Errorf("Invalid multipathd command output received")
}

func isSupportedDeviceVendor(deviceVendors []string, vendor string) bool {
	for _, value := range deviceVendors {
		if value == vendor {
			return true
		}
	}
	return false
}

func RemoveBlockDevicesOfMultipathDevices(device model.MultipathDevice) error {
	log.Trace(">>>> RemoveBlockDevicesOfMultipathDevices")
	defer log.Trace("<<<<< RemoveBlockDevicesOfMultipathDevices")

	blockDevices := getBlockDevicesOfMultipathDevice(device)
	if len(blockDevices) == 0 {
		log.Infof("No block devices were found for the multipath device %s", device.Name)
		return nil
	}
	log.Infof("%d block devices found for the multipath device %s", len(blockDevices), device.Name)
	err := removeBlockDevices(blockDevices, device.Name)
	if err != nil {
		log.Errorf("Error occurred while removing the block devices of the  multipath device %s", device.Name)
		return err
	}
	log.Infof("Block devices of the multipath device %s are removed successfully.", device.Name)
	return nil
}

func getBlockDevicesOfMultipathDevice(device model.MultipathDevice) (blockDevices []string) {
	log.Tracef(">>>> getBlockDevicesOfMultipathDevice: %+v", device)
	defer log.Trace("<<<<< getBlockDevicesOfMultipathDevice")

	if len(device.PathGroups) > 0 {
		for _, pathGroup := range device.PathGroups {
			if len(pathGroup.Paths) > 0 {
				for _, path := range pathGroup.Paths {
					blockDevices = append(blockDevices, path.Dev)
				}
			}
		}
	}
	return blockDevices
}

func removeBlockDevices(blockDevices []string, multipathDevice string) error {
	log.Trace(">>>> removeBlockDevices: ", blockDevices)
	defer log.Trace("<<<<< removeBlockDevices")
	for _, blockDevice := range blockDevices {
		log.Debugf("Removing the block device %s of the multipath device %s", blockDevice, multipathDevice)
		cmd := exec.Command("sh", "-c", "echo 1 > /sys/block/"+blockDevice+"/device/delete")
		err := cmd.Run()
		if err != nil {
			log.Errorf("Error occurred while deleting the block device %s of the multipath device %s: %s", blockDevice, multipathDevice, err.Error())
			return err
		}
	}
	return nil
}

func UnmountMultipathDevice(multipathDevice string) error {
	log.Tracef(">>>> UnmountMultipathDevice: %s", multipathDevice)
	defer log.Trace("<<<<< UnmountMultipathDevice")

	mountPoints, err := findMountPointsOfMultipathDevice(multipathDevice)
	if err != nil {
		return fmt.Errorf("Error occurred while fetching the mount points of the multipath device %s:%s", multipathDevice, err.Error())
	}
	if len(mountPoints) == 0 {
		log.Infof("No mount points found for the multipath device %s", multipathDevice)
		return nil
	}

	umountMutex.Lock()
	defer umountMutex.Unlock()

	for _, mountPoint := range mountPoints {
		err = unmount(mountPoint)
		if err != nil {
			log.Warnf("Error occurred while unmounting %s. Trying to find processes using this mount point...", mountPoint)
			err = KillProcessesUsingMountPoints(mountPoint)
			if err != nil {
				return fmt.Errorf("Unable to kill the processes using the mount point %s: %s", mountPoint, err.Error())
			}
			log.Debugf("Retrying to unmount the mount point %s after killing the processes using it", mountPoint)
			err = unmount(mountPoint)
			if err != nil {
				log.Errorf("Failed to unmount the mount point %s even though the processes are killed.", mountPoint)
				return err
			}
		}
		log.Debugf("Mount point %s unmounted successfully.", mountPoint)
	}
	return nil
}

func unmount(mountPoint string) error {
	log.Tracef("Unmount the mount point %s", mountPoint)

	args := []string{mountPoint}
	_, rc, err := util.ExecCommandOutput("umount", args)
	if err != nil || rc != 0 {
		log.Errorf("Error occurred while unmounting the mount point %s: %s", mountPoint, err.Error())
		return err
	}
	return nil
}

func KillProcessesUsingMountPoints(mountPoint string) error {
	log.Tracef(">>>> KillProcessesUsingMountPoints: %s", mountPoint)
	defer log.Trace("<<<<< KillProcessesUsingMountPoints")

	staleDeviceRemovalMutex.Lock()
	defer staleDeviceRemovalMutex.Unlock()

	args := []string{"-mv", mountPoint}
	output, _, err := util.ExecCommandOutput("fuser", args)
	if err != nil {
		log.Errorf("Either no processes are using the mount point/device %s or unable to list the processes using the mount point/device %s using the fuser command: %s", mountPoint, err.Error())
		return err
	}

	lines := strings.Split(output, "\n")
	if len(lines) > 2 {
		// Skip the first line which contains the headers
		for _, line := range lines[1:] {
			if strings.TrimSpace(line) == "" {
				continue
			}
			// Skip the line if it conatains kernel, as it is not needed
			if strings.Contains(line, "kernel") {
				continue
			}
			lineItems := strings.Fields(string(line))
			if len(lineItems) > 1 {
				pidStr := ""
				if len(lineItems) > 4 && lineItems[2] != "" {
					pidStr = lineItems[2]
				} else if len(lineItems) == 4 && lineItems[1] != "" {
					pidStr = lineItems[1]
				}
				if len(pidStr) == 0 {
					continue
				}
				pid, err := strconv.Atoi(pidStr)
				if err != nil {
					log.Errorf("Error converting the PID of the process using the mount point %s:%s", mountPoint, err.Error())
					continue
				}
				log.Tracef("PROCESS ID: %d using the mount point/device: %s", pid, mountPoint)
				if pid > 0 {
					if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
						log.Errorf("Error killing process %d: %s", pid, err.Error())
						return err
					} else {
						log.Tracef("Process %d killed", pid)
					}
				}
			} else {
				return fmt.Errorf("Improper output received while getting the list of processes accesing the mount point/device %s", mountPoint)
			}
		}
	} else if len(lines) == 2 {
		log.Debugf("No process is using the mount point/device %s", mountPoint)
	} else {
		return fmt.Errorf("Improper output received while getting the list of processes accessing the mount point/device %s", mountPoint)
	}
	return nil
}

func findMountPointsOfMultipathDevice(multipathDevice string) (mountPoints []string, err error) {
	log.Tracef(">>>> findMountPointsOfMultipathDevice: %s", multipathDevice)
	defer log.Trace("<<<<< findMountPointsOfMultipathDevice")

	// take a lock on access mounts
	mountMutex.Lock()
	defer mountMutex.Unlock()

	var args []string
	out, _, err := util.ExecCommandOutput("mount", args)
	if err != nil {
		return mountPoints, err
	}

	mountLines := strings.Split(out, "\n")
	log.Tracef("Number of mounts retrieved %d", len(mountLines))

	for _, line := range mountLines {
		entry := strings.Fields(line)
		log.Trace("mounts entry :", entry)
		if len(entry) > 3 {
			if strings.Contains(entry[0], multipathDevice) {
				log.Tracef("%s was found with %s", multipathDevice, entry[2])
				mountPoints = append(mountPoints, entry[2])
			}
		}
	}
	return mountPoints, nil
}

func FlushMultipathDevice(multipathDevice string) error {
	log.Tracef(">>>> FlushMultipathDevice: %s", multipathDevice)
	defer log.Trace("<<<<< FlushMultipathDevice")

	staleDeviceRemovalMutex.Lock()
	defer staleDeviceRemovalMutex.Unlock()

	_, _, err := util.ExecCommandOutput("multipath", []string{"-f", multipathDevice})
	if err != nil {
		log.Errorf("Error occurred while removing the multipath device %s: %s", multipathDevice, err.Error())

		log.Infof("Trying to remove the multipath device %s using the dmsetup command", multipathDevice)
		err = displayDeviceInfo(multipathDevice)
		if err != nil {
			log.Errorf("Error while displaying the device info %s", multipathDevice)
		}
		err = listTheProcessesUsingDevice(multipathDevice)
		if err != nil {
			log.Errorf("Error while displaying the processes using the device info %s", multipathDevice)
		}
		err = forceDeleteMultipathDevice(multipathDevice)
		if err != nil {
			return fmt.Errorf("Unable to remove the multipath device %s by force as well: %s", multipathDevice, err.Error())
		}

		return err
	}
	log.Debugf("Multipath device %s is removed successfully.", multipathDevice)
	return nil
}

func listTheProcessesUsingDevice(multipathDevice string) error {
	log.Tracef(">>>> listTheProcessesUsingDevice: %s", multipathDevice)
	args := []string{"-mv", "/dev/mapper/" + multipathDevice}
	output, _, err := util.ExecCommandOutput("fuser", args)
	if err != nil {
		log.Errorf("Either no processes are using the device %s or unable to list the processes using the mount point %s using the fuser command: %s", multipathDevice, multipathDevice, err.Error())
		return err
	}
	log.Infof("Processes using the multipath device %s are:", multipathDevice)
	log.Infof(output)
	return nil
}
func displayDeviceInfo(multipathDevice string) error {
	log.Tracef(">>>> displayDeviceInfo: %s", multipathDevice)
	out, _, err := util.ExecCommandOutput("dmsetup", []string{"info", multipathDevice})
	if err != nil {
		log.Errorf("Error occurred while removing the multipath device %s by force: %s", multipathDevice, err.Error())
		return err
	}
	log.Infof("Device info of multipath device %s using the dmsetup info command: %s", multipathDevice, out)
	return nil
}
func forceDeleteMultipathDevice(multipathDevice string) error {
	log.Tracef(">>>> forceDeleteMultipathDevice: %s", multipathDevice)
	defer log.Trace("<<<<< forceDeleteMultipathDevice")

	_, _, err := util.ExecCommandOutput("dmsetup", []string{"remove", "-f", multipathDevice})
	if err != nil {
		log.Errorf("Error occurred while removing the multipath device %s by force: %s", multipathDevice, err.Error())
		return err
	}
	return nil
}
