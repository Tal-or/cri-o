package runtimehandlerhooks

import (
	"context"
	"errors"
	"fmt"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cri-o/cri-o/internal/config/cgmgr"
	"github.com/cri-o/cri-o/internal/config/node"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/internal/oci"
	crioannotations "github.com/cri-o/cri-o/pkg/annotations"
	"github.com/cri-o/cri-o/utils/cmdrunner"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	libCtrMgr "github.com/opencontainers/runc/libcontainer/cgroups/manager"
	"github.com/opencontainers/runc/libcontainer/configs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/fields"
)

const (
	// HighPerformance contains the high-performance runtime handler name
	HighPerformance = "high-performance"
	// IrqSmpAffinityProcFile contains the default smp affinity mask configuration
	IrqSmpAffinityProcFile = "/proc/irq/default_smp_affinity"
)

const (
	annotationTrue       = "true"
	annotationDisable    = "disable"
	annotationEnable     = "enable"
	schedDomainDir       = "/proc/sys/kernel/sched_domain"
	cgroupMountPoint     = "/sys/fs/cgroup"
	irqBalanceBannedCpus = "IRQBALANCE_BANNED_CPUS"
	irqBalancedName      = "irqbalance"
	sysCPUDir            = "/sys/devices/system/cpu"
	sysCPUSaveDir        = "/var/run/crio/cpu"
	milliCPUToCPU        = 1000
)

const (
	cpusetPartition      = "cpuset.cpus.partition"
	cpusetExclusive      = "cpuset.cpus.exclusive"
	cpusetCpus           = "cpuset.cpus"
	cgroupSubTreeControl = "cgroup.subtree_control"
	cgroupV1Quota        = "cpu.cfs_quota_us"
	cgroupV2Quota        = "cpu.max"
)

const (
	IsolatedCPUsEnvVar = "OPENSHIFT_ISOLATED_CPUS"
	SharedCPUsEnvVar   = "OPENSHIFT_SHARED_CPUS"
)

// HighPerformanceHooks used to run additional hooks that will configure a system for the latency sensitive workloads
type HighPerformanceHooks struct {
	irqBalanceConfigFile string
	sharedCPUs           *cpuset.CPUSet
}

func (h *HighPerformanceHooks) PreStart(ctx context.Context, c *oci.Container, s *sandbox.Sandbox) error {
	log.Infof(ctx, "Run %q runtime handler pre-start hook for the container %q", HighPerformance, c.ID())

	cSpec := c.Spec()
	if !shouldRunHooks(ctx, c.ID(), &cSpec, s) {
		return nil
	}

	// disable the CPU load balancing for the container CPUs
	if shouldCPULoadBalancingBeDisabled(s.Annotations()) {
		if err := setCPULoadBalancing(c, false); err != nil {
			return fmt.Errorf("set CPU load balancing: %w", err)
		}
	}

	if requestedSharedCPUs(s.Annotations(), c.CRIContainer().GetMetadata().GetName()) {
		if err := setSharedCPUs(ctx, c, s, h.sharedCPUs); err != nil {
			return fmt.Errorf("set shared CPUs: %w", err)
		}
	}

	// disable the IRQ smp load balancing for the container CPUs
	if shouldIRQLoadBalancingBeDisabled(s.Annotations()) {
		log.Infof(ctx, "Disable irq smp balancing for container %q", c.ID())
		if err := setIRQLoadBalancing(ctx, c, false, IrqSmpAffinityProcFile, h.irqBalanceConfigFile); err != nil {
			return fmt.Errorf("set IRQ load balancing: %w", err)
		}
	}

	// disable the CFS quota for the container CPUs
	if shouldCPUQuotaBeDisabled(s.Annotations()) {
		log.Infof(ctx, "Disable cpu cfs quota for container %q", c.ID())
		if err := setCPUQuota(s.CgroupParent(), c); err != nil {
			return fmt.Errorf("set CPU CFS quota: %w", err)
		}
	}

	// Configure c-states for the container CPUs.
	if configure, value := shouldCStatesBeConfigured(s.Annotations()); configure {
		log.Infof(ctx, "Configure c-states for container %q to %q", c.ID(), value)
		switch value {
		case annotationEnable:
			// Enable all c-states.
			if err := setCPUPMQOSResumeLatency(c, "0"); err != nil {
				return fmt.Errorf("set CPU PM QOS resume latency: %w", err)
			}
		case annotationDisable:
			// Lock the c-state to C0.
			if err := setCPUPMQOSResumeLatency(c, "n/a"); err != nil {
				return fmt.Errorf("set CPU PM QOS resume latency: %w", err)
			}
		default:
			return fmt.Errorf("invalid annotation value %s", value)
		}
	}

	// Configure cpu freq governor for the container CPUs.
	if configure, value := shouldFreqGovernorBeConfigured(s.Annotations()); configure {
		log.Infof(ctx, "Configure cpu freq governor for container %q to %q", c.ID(), value)
		// Set the cpu freq governor to specified value.
		if err := setCPUFreqGovernor(c, value); err != nil {
			return fmt.Errorf("set CPU scaling governor: %w", err)
		}
	}

	return nil
}

func (h *HighPerformanceHooks) PreStop(ctx context.Context, c *oci.Container, s *sandbox.Sandbox) error {
	ctx, span := log.StartSpan(ctx)
	defer span.End()
	log.Infof(ctx, "Run %q runtime handler pre-stop hook for the container %q", HighPerformance, c.ID())

	cSpec := c.Spec()
	if !shouldRunHooks(ctx, c.ID(), &cSpec, s) {
		return nil
	}

	// enable the IRQ smp balancing for the container CPUs
	if shouldIRQLoadBalancingBeDisabled(s.Annotations()) {
		if err := setIRQLoadBalancing(ctx, c, true, IrqSmpAffinityProcFile, h.irqBalanceConfigFile); err != nil {
			return fmt.Errorf("set IRQ load balancing: %w", err)
		}
	}

	// disable the CPU load balancing for the container CPUs
	if shouldCPULoadBalancingBeDisabled(s.Annotations()) {
		if err := setCPULoadBalancing(c, true); err != nil {
			return fmt.Errorf("set CPU load balancing: %w", err)
		}
	}

	// no need to reverse the cgroup CPU CFS quota setting as the pod cgroup will be deleted anyway

	// Restore the c-state configuration for the container CPUs (only do this when the annotation is
	// present - without the annotation we do not modify the c-state).
	if configure, _ := shouldCStatesBeConfigured(s.Annotations()); configure {
		// Restore the original resume latency value.
		if err := setCPUPMQOSResumeLatency(c, ""); err != nil {
			return fmt.Errorf("set CPU PM QOS resume latency: %w", err)
		}
	}

	// Restore the cpu freq governor for the container CPUs (only do this when the annotation is
	// present - without the annotation we do not modify the governor).
	if configure, _ := shouldFreqGovernorBeConfigured(s.Annotations()); configure {
		// Restore the original scaling governor.
		if err := setCPUFreqGovernor(c, ""); err != nil {
			return fmt.Errorf("set CPU scaling governor: %w", err)
		}
	}

	return nil
}

// If CPU load balancing is enabled, then *all* containers must run this PostStop hook.
func (*HighPerformanceHooks) PostStop(ctx context.Context, c *oci.Container, s *sandbox.Sandbox) error {
	// We could check if `!cpuLoadBalancingAllowed()` here, but it requires access to the config, which would be
	// odd to plumb. Instead, always assume if they're using a HighPerformanceHook, they have CPULoadBalanceDisabled
	// annotation allowed.
	h := &DefaultCPULoadBalanceHooks{}
	return h.PostStop(ctx, c, s)
}

func shouldCPULoadBalancingBeDisabled(annotations fields.Set) bool {
	if annotations[crioannotations.CPULoadBalancingAnnotation] == annotationTrue {
		log.Warnf(context.TODO(), annotationValueDeprecationWarning(crioannotations.CPULoadBalancingAnnotation))
	}

	return annotations[crioannotations.CPULoadBalancingAnnotation] == annotationTrue ||
		annotations[crioannotations.CPULoadBalancingAnnotation] == annotationDisable
}

func shouldCPUQuotaBeDisabled(annotations fields.Set) bool {
	if annotations[crioannotations.CPUQuotaAnnotation] == annotationTrue {
		log.Warnf(context.TODO(), annotationValueDeprecationWarning(crioannotations.CPUQuotaAnnotation))
	}

	return annotations[crioannotations.CPUQuotaAnnotation] == annotationTrue ||
		annotations[crioannotations.CPUQuotaAnnotation] == annotationDisable
}

func shouldIRQLoadBalancingBeDisabled(annotations fields.Set) bool {
	if annotations[crioannotations.IRQLoadBalancingAnnotation] == annotationTrue {
		log.Warnf(context.TODO(), annotationValueDeprecationWarning(crioannotations.IRQLoadBalancingAnnotation))
	}

	return annotations[crioannotations.IRQLoadBalancingAnnotation] == annotationTrue ||
		annotations[crioannotations.IRQLoadBalancingAnnotation] == annotationDisable
}

func shouldCStatesBeConfigured(annotations fields.Set) (present bool, value string) {
	value, present = annotations[crioannotations.CPUCStatesAnnotation]
	return
}

func shouldFreqGovernorBeConfigured(annotations fields.Set) (present bool, value string) {
	value, present = annotations[crioannotations.CPUFreqGovernorAnnotation]
	return
}

func annotationValueDeprecationWarning(annotation string) string {
	return fmt.Sprintf("The usage of the annotation %q with value %q will be deprecated under 1.21", annotation, "true")
}

func requestedSharedCPUs(annotations fields.Set, cName string) bool {
	key := crioannotations.CPUSharedAnnotation + "/" + cName
	v, ok := annotations[key]
	return ok && v == annotationEnable
}

// setCPULoadBalancing relies on the cpuset cgroup to disable load balancing for containers.
func setCPULoadBalancing(c *oci.Container, enable bool) error {
	lspec := c.Spec().Linux
	if lspec == nil ||
		lspec.Resources == nil ||
		lspec.Resources.CPU == nil ||
		lspec.Resources.CPU.Cpus == "" {
		return fmt.Errorf("find container %s CPUs", c.ID())
	}

	if node.CgroupIsV2() {
		return setCPULoadBalancingV2(c, enable)
	}
	if !enable {
		return disableCPULoadBalancingV1(c)
	}
	// There is nothing to do in cgroupv1 to re-enable load balancing
	return nil
}

// On cgroupv2 systems, a new kernel API has been added to support load balancing
// in a "remote" partition, layers away from the root cgroup.
// This is done with a special file `cpuset.cpus.exclusive` which can be written to
// to request that cpuset be on standby for use by a cgroup that desires to be a partition.
// To do this, each parent of the final cgroup must also have this value in the cpuset.cpus.exclusive,
// and the final cgroup must have cpuset.cpus.partition = isolated.
// This will cause the kernel to put that cpuset in a separate scheduling domain.
// While this requires CRI-O to write to cgroups it does not own, it would be cumbersome to teach
// other components in the system (kubelet/cpumanager) which cpu is newly set to exclusive each time
// a pod request load balancing disabled.
// Thus, this implementation assumes a certain amount of ownership CRI-O takes over this field. This
// ownership may not be real in the future.
func setCPULoadBalancingV2(c *oci.Container, enable bool) (retErr error) {
	cpus, err := cpuset.Parse(c.Spec().Linux.Resources.CPU.Cpus)
	if err != nil {
		return err
	}

	cpusetPath, err := cpusetOfContainer(c)
	if err != nil {
		return err
	}

	// For each parent (excluding the root), the cpuset.cpus.exclusive
	// must contain the required cgroup.
	directories := strings.Split(cpusetPath, "/")
	currentPath := "/sys/fs/cgroup"

	// Save the old values of the cpuset.cpus.exclusive, so if this fails in the middle,
	// the newly reserved cpu won't be in a subset of the cgroups and cause them to not be
	// load balanced in the future.
	valuesForRevert := make(map[string]string)
	defer func() {
		if retErr != nil {
			for path, val := range valuesForRevert {
				if err := cgroups.WriteFile(path, "cpuset.cpus.exclusive", val); err != nil {
					logrus.Errorf("Failed to revert cpuset value %s for path %s: %v", val, path, err)
				}
			}
		}
	}()

	for _, d := range directories {
		if d == "" {
			continue
		}
		currentPath += "/" + d
		if !enable {
			// if we're disabling, double check the cpuset.cpus are correctly set, or else we'll get EINVAL
			if _, err := addOrRemoveCpusetFromFile(currentPath, "cpuset.cpus", cpus, !enable); err != nil {
				return err
			}
		}
		cpusStrForRevert, err := addOrRemoveCpusetFromFile(currentPath, "cpuset.cpus.exclusive", cpus, !enable)
		if err != nil {
			return err
		}
		valuesForRevert[currentPath] = cpusStrForRevert
	}
	err = cgroups.WriteFile(filepath.Join("/sys/fs/cgroup", cpusetPath), "cpuset.cpus.partition", "isolated")
	// If we're re-enabling, and we can't find the cgroup, return no error.
	if os.IsNotExist(err) && enable {
		return nil
	}
	return err
}

func addOrRemoveCpusetFromFile(path, file string, cpus cpuset.CPUSet, add bool) (cpusStrForRevert string, _ error) {
	currentCpusStr, err := cgroups.ReadFile(path, file)
	if err != nil {
		return "", err
	}
	currentCpus, err := cpuset.Parse(strings.TrimSpace(currentCpusStr))
	if err != nil {
		return "", err
	}

	targetCpus := cpuset.CPUSet{}
	if add {
		targetCpus = currentCpus.Union(cpus)
	} else {
		targetCpus = currentCpus.Difference(cpus)
	}
	if err := cgroups.WriteFile(path, file, targetCpus.String()); err != nil {
		return "", err
	}
	return currentCpusStr, nil
}

// The requisite condition to allow this is `cpuset.sched_load_balance` field must be set to 0 for all cgroups
// that intersect with `cpuset.cpus` of the container that desires load balancing.
// Since CRI-O is the owner of the container cgroup, it must set this value for
// the container. Some other entity (kubelet, external service) must ensure this is the case for all
// other cgroups that intersect (at minimum: all parent cgroups of this cgroup).
func disableCPULoadBalancingV1(c *oci.Container) error {
	cpusetPath, err := cpusetOfContainer(c)
	if err != nil {
		return err
	}
	return cgroups.WriteFile("/sys/fs/cgroup/cpuset"+cpusetPath, "cpuset.sched_load_balance", "0")
}

func cpusetOfContainer(c *oci.Container) (string, error) {
	pid, err := c.Pid()
	if err != nil {
		return "", fmt.Errorf("failed to get pid of container %s: %w", c.ID(), err)
	}
	controllers, err := cgroups.ParseCgroupFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return "", fmt.Errorf("failed to get cgroups of container %s: %w", c.ID(), err)
	}

	path := "cpuset"
	if node.CgroupIsV2() {
		path = ""
	}

	cpusetPath, ok := controllers[path]
	if !ok {
		return "", fmt.Errorf("failed to get cpuset of container %s", c.ID())
	}

	return cpusetPath, nil
}

func setIRQLoadBalancing(ctx context.Context, c *oci.Container, enable bool, irqSmpAffinityFile, irqBalanceConfigFile string) error {
	lspec := c.Spec().Linux
	if lspec == nil ||
		lspec.Resources == nil ||
		lspec.Resources.CPU == nil ||
		lspec.Resources.CPU.Cpus == "" {
		return fmt.Errorf("find container %s CPUs", c.ID())
	}

	content, err := os.ReadFile(irqSmpAffinityFile)
	if err != nil {
		return err
	}
	currentIRQSMPSetting := strings.TrimSpace(string(content))
	newIRQSMPSetting, newIRQBalanceSetting, err := UpdateIRQSmpAffinityMask(lspec.Resources.CPU.Cpus, currentIRQSMPSetting, enable)
	if err != nil {
		return err
	}
	if err := os.WriteFile(irqSmpAffinityFile, []byte(newIRQSMPSetting), 0o644); err != nil {
		return err
	}

	isIrqConfigExists := fileExists(irqBalanceConfigFile)

	if isIrqConfigExists {
		if err := updateIrqBalanceConfigFile(irqBalanceConfigFile, newIRQBalanceSetting); err != nil {
			return err
		}
	}

	if !isServiceEnabled(irqBalancedName) || !isIrqConfigExists {
		if _, err := exec.LookPath(irqBalancedName); err != nil {
			// irqbalance is not installed, skip the rest; pod should still start, so return nil instead
			log.Warnf(ctx, "Irqbalance binary not found: %v", err)
			return nil
		}
		// run irqbalance in daemon mode, so this won't cause delay
		cmd := cmdrunner.Command(irqBalancedName, "--oneshot")
		additionalEnv := irqBalanceBannedCpus + "=" + newIRQBalanceSetting
		cmd.Env = append(os.Environ(), additionalEnv)
		return cmd.Run()
	}

	if err := restartIrqBalanceService(); err != nil {
		log.Warnf(ctx, "Irqbalance service restart failed: %v", err)
	}
	return nil
}

func setCPUQuota(parentDir string, c *oci.Container) error {
	containerCgroup, containerCgroupParent, systemd, err := containerCgroupAndParent(parentDir, c)
	if err != nil {
		return err
	}
	podCgroup := filepath.Base(containerCgroupParent)
	podCgroupParent := filepath.Dir(containerCgroupParent)

	if err := disableCPUQuotaForCgroup(podCgroup, podCgroupParent, systemd); err != nil {
		return err
	}
	return disableCPUQuotaForCgroup(containerCgroup, containerCgroupParent, systemd)
}

func containerCgroupAndParent(parentDir string, c *oci.Container) (ctrCgroup, parentCgroup string, systemd bool, _ error) {
	var (
		cgroupManager cgmgr.CgroupManager
		err           error
	)

	if strings.HasSuffix(parentDir, ".slice") {
		if cgroupManager, err = cgmgr.SetCgroupManager("systemd"); err != nil {
			// Programming error, this is only possible if the manager string is invalid.
			panic(err)
		}
	} else if cgroupManager, err = cgmgr.SetCgroupManager("cgroupfs"); err != nil {
		// Programming error, this is only possible if the manager string is invalid.
		panic(err)
	}
	cgroupPath, err := cgroupManager.ContainerCgroupAbsolutePath(parentDir, c.ID())
	if err != nil {
		return "", "", false, err
	}
	containerCgroup := filepath.Base(cgroupPath)
	// A quirk of libcontainer's cgroup driver.
	// See explanation in disableCPUQuotaForCgroup function.
	if cgroupManager.IsSystemd() {
		containerCgroup = c.ID()
	}
	return containerCgroup, filepath.Dir(cgroupPath), cgroupManager.IsSystemd(), nil
}

func disableCPUQuotaForCgroup(cgroup, parent string, systemd bool) error {
	mgr, err := libctrManager(cgroup, parent, systemd)
	if err != nil {
		return err
	}

	return mgr.Set(&configs.Resources{
		SkipDevices: true,
		CpuQuota:    -1,
	})
}

func libctrManager(cgroup, parent string, systemd bool) (cgroups.Manager, error) {
	if systemd {
		parent = filepath.Base(parent)
	}
	cg := &configs.Cgroup{
		Name:   cgroup,
		Parent: parent,
		Resources: &configs.Resources{
			SkipDevices: true,
		},
		Systemd: systemd,
		// If the cgroup manager is systemd, then libcontainer
		// will construct the cgroup path (for scopes) as:
		// ScopePrefix-Name.scope. For slices, and for cgroupfs manager,
		// this will be ignored.
		// See: https://github.com/opencontainers/runc/tree/main/libcontainer/cgroups/systemd/common.go:getUnitName
		ScopePrefix: cgmgr.CrioPrefix,
	}
	return libCtrMgr.New(cg)
}

// setCPUPMQOSResumeLatency sets the pm_qos_resume_latency_us for a cpu and stores the original
// value so it can be restored later. If the latency is an empty string, the original latency
// value is restored.
func setCPUPMQOSResumeLatency(c *oci.Container, latency string) error {
	return doSetCPUPMQOSResumeLatency(c, latency, sysCPUDir, sysCPUSaveDir)
}

// doSetCPUPMQOSResumeLatency facilitates unit testing by allowing the directories to be specified as parameters.
func doSetCPUPMQOSResumeLatency(c *oci.Container, latency, cpuDir, cpuSaveDir string) error {
	lspec := c.Spec().Linux
	if lspec == nil ||
		lspec.Resources == nil ||
		lspec.Resources.CPU == nil ||
		lspec.Resources.CPU.Cpus == "" {
		return fmt.Errorf("find container %s CPUs", c.ID())
	}

	cpus, err := cpuset.Parse(lspec.Resources.CPU.Cpus)
	if err != nil {
		return err
	}

	for _, cpu := range cpus.List() {
		latencyFile := fmt.Sprintf("%s/cpu%d/power/pm_qos_resume_latency_us", cpuDir, cpu)
		cpuPowerSaveDir := fmt.Sprintf("%s/cpu%d/power", cpuSaveDir, cpu)
		latencyFileOrig := path.Join(cpuPowerSaveDir, "pm_qos_resume_latency_us")

		if latency != "" {
			// Retrieve the current latency.
			latencyOrig, err := os.ReadFile(latencyFile)
			if err != nil {
				return err
			}

			// Save the current latency so we can restore it later.
			err = os.MkdirAll(cpuPowerSaveDir, 0o750)
			if err != nil {
				return err
			}
			err = os.WriteFile(latencyFileOrig, latencyOrig, 0o644)
			if err != nil {
				return err
			}

			// Update the pm_qos_resume_latency_us.
			err = os.WriteFile(latencyFile, []byte(latency), 0o644)
			if err != nil {
				return err
			}

			continue
		}

		// Retrieve the original latency.
		latencyOrig, err := os.ReadFile(latencyFileOrig)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// The latency may have already been restored by a previous invocation of the hook.
				return nil
			}
			return err
		}

		// Restore the original latency.
		err = os.WriteFile(latencyFile, latencyOrig, 0o644)
		if err != nil {
			return err
		}

		// Remove the saved latency.
		err = os.Remove(latencyFileOrig)
		if err != nil {
			return err
		}
	}

	return nil
}

// isCPUGovernorSupported checks whether the cpu governor is supported for the specified cpu.
func isCPUGovernorSupported(governor, cpuDir string, cpu int) error {
	// Get available cpu scaling governors.
	availGovernorFile := fmt.Sprintf("%s/cpu%d/cpufreq/scaling_available_governors", cpuDir, cpu)
	availGovernors, err := os.ReadFile(availGovernorFile)
	if err != nil {
		return err
	}

	// Is the scaling governor supported?
	for _, availableGovernor := range strings.Fields(string(availGovernors)) {
		if availableGovernor == governor {
			return nil
		}
	}

	return fmt.Errorf("governor %s not available for cpu %d", governor, cpu)
}

// setCPUFreqGovernor sets the scaling_governor for a cpu and stores the original
// value so it can be restored later. If the governor is an empty string, the original
// scaling_governor value is restored.
func setCPUFreqGovernor(c *oci.Container, governor string) error {
	return doSetCPUFreqGovernor(c, governor, sysCPUDir, sysCPUSaveDir)
}

// doSetCPUFreqGovernor facilitates unit testing by allowing the directories to be specified as parameters.
func doSetCPUFreqGovernor(c *oci.Container, governor, cpuDir, cpuSaveDir string) error {
	lspec := c.Spec().Linux
	if lspec == nil ||
		lspec.Resources == nil ||
		lspec.Resources.CPU == nil ||
		lspec.Resources.CPU.Cpus == "" {
		return fmt.Errorf("find container %s CPUs", c.ID())
	}

	cpus, err := cpuset.Parse(lspec.Resources.CPU.Cpus)
	if err != nil {
		return err
	}

	for _, cpu := range cpus.List() {
		governorFile := fmt.Sprintf("%s/cpu%d/cpufreq/scaling_governor", cpuDir, cpu)
		cpuFreqSaveDir := fmt.Sprintf("%s/cpu%d/cpufreq", cpuSaveDir, cpu)
		governorFileOrig := path.Join(cpuFreqSaveDir, "scaling_governor")

		if governor != "" {
			// Retrieve the current scaling governor.
			governorOrig, err := os.ReadFile(governorFile)
			if err != nil {
				return err
			}

			// Is the scaling governor supported?
			if err := isCPUGovernorSupported(governor, cpuDir, cpu); err != nil {
				return err
			}

			// Save the current governor so we can restore it later.
			err = os.MkdirAll(cpuFreqSaveDir, 0o750)
			if err != nil {
				return err
			}
			err = os.WriteFile(governorFileOrig, governorOrig, 0o644)
			if err != nil {
				return err
			}

			// Update the governor.
			err = os.WriteFile(governorFile, []byte(governor), 0o644)
			if err != nil {
				return err
			}

			continue
		}

		// Retrieve the original scaling governor.
		governorOrig, err := os.ReadFile(governorFileOrig)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// The governor may have already been restored by a previous invocation of the hook.
				return nil
			}
			return err
		}

		// Restore the original governor.
		err = os.WriteFile(governorFile, governorOrig, 0o644)
		if err != nil {
			return err
		}

		// Remove the saved governor.
		err = os.Remove(governorFileOrig)
		if err != nil {
			return err
		}
	}

	return nil
}

// RestoreIrqBalanceConfig restores irqbalance service with original banned cpu mask settings
func RestoreIrqBalanceConfig(ctx context.Context, irqBalanceConfigFile, irqBannedCPUConfigFile, irqSmpAffinityProcFile string) error {
	content, err := os.ReadFile(irqSmpAffinityProcFile)
	if err != nil {
		return err
	}
	current := strings.TrimSpace(string(content))
	// remove ","; now each element is "0-9,a-f"
	s := strings.ReplaceAll(current, ",", "")
	currentMaskArray, err := mapHexCharToByte(s)
	if err != nil {
		return err
	}
	if !isAllBitSet(currentMaskArray) {
		// not system reboot scenario, just return it.
		log.Infof(ctx, "Restore irqbalance config: not system reboot, ignoring")
		return nil
	}

	bannedCPUMasks, err := retrieveIrqBannedCPUMasks(irqBalanceConfigFile)
	if err != nil {
		// Ignore returning err as given irqBalanceConfigFile may not exist.
		log.Infof(ctx, "Restore irqbalance config: failed to get current CPU ban list, ignoring")
		return nil
	}

	if !fileExists(irqBannedCPUConfigFile) {
		log.Infof(ctx, "Creating banned CPU list file %q", irqBannedCPUConfigFile)
		irqBannedCPUsConfig, err := os.Create(irqBannedCPUConfigFile)
		if err != nil {
			return err
		}
		defer irqBannedCPUsConfig.Close()
		_, err = irqBannedCPUsConfig.WriteString(bannedCPUMasks)
		if err != nil {
			return err
		}
		log.Infof(ctx, "Restore irqbalance config: created backup file")
		return nil
	}

	content, err = os.ReadFile(irqBannedCPUConfigFile)
	if err != nil {
		return err
	}
	origBannedCPUMasks := strings.TrimSpace(string(content))

	if bannedCPUMasks == origBannedCPUMasks {
		log.Infof(ctx, "Restore irqbalance config: nothing to do")
		return nil
	}

	log.Infof(ctx, "Restore irqbalance banned CPU list in %q to %q", irqBalanceConfigFile, origBannedCPUMasks)
	if err := updateIrqBalanceConfigFile(irqBalanceConfigFile, origBannedCPUMasks); err != nil {
		return err
	}
	if isServiceEnabled(irqBalancedName) {
		if err := restartIrqBalanceService(); err != nil {
			log.Warnf(ctx, "Irqbalance service restart failed: %v", err)
		}
	}
	return nil
}

func ShouldCPUQuotaBeDisabled(ctx context.Context, cid string, cSpec *specs.Spec, s *sandbox.Sandbox, annotations fields.Set) bool {
	if !shouldRunHooks(ctx, cid, cSpec, s) {
		return false
	}
	if annotations[crioannotations.CPUQuotaAnnotation] == annotationTrue {
		log.Warnf(context.TODO(), annotationValueDeprecationWarning(crioannotations.CPUQuotaAnnotation))
	}

	return annotations[crioannotations.CPUQuotaAnnotation] == annotationTrue ||
		annotations[crioannotations.CPUQuotaAnnotation] == annotationDisable
}

func shouldRunHooks(ctx context.Context, id string, cSpec *specs.Spec, s *sandbox.Sandbox) bool {
	if isCgroupParentBurstable(s) {
		log.Infof(ctx, "Container %q is a burstable pod. Skip PreStart.", id)
		return false
	}
	if isCgroupParentBestEffort(s) {
		log.Infof(ctx, "Container %q is a besteffort pod. Skip PreStart.", id)
		return false
	}
	if !isContainerRequestWholeCPU(cSpec) {
		log.Infof(ctx, "Container %q requests partial cpu(s). Skip PreStart", id)
		return false
	}
	return true
}

func isCgroupParentBurstable(s *sandbox.Sandbox) bool {
	return strings.Contains(s.CgroupParent(), "burstable")
}

func isCgroupParentBestEffort(s *sandbox.Sandbox) bool {
	return strings.Contains(s.CgroupParent(), "besteffort")
}

func isContainerRequestWholeCPU(cSpec *specs.Spec) bool {
	return *(cSpec.Linux.Resources.CPU.Shares)%1024 == 0
}

func setSharedCPUs(ctx context.Context, c *oci.Container, s *sandbox.Sandbox, sharedCPUs *cpuset.CPUSet) error {
	lspec := c.Spec().Linux
	if lspec == nil ||
		lspec.Resources == nil ||
		lspec.Resources.CPU == nil ||
		lspec.Resources.CPU.Cpus == "" {
		return fmt.Errorf("no cpus found for container %q", c.Name())
	}
	isolatedCPUs, err := cpuset.Parse(lspec.Resources.CPU.Cpus)
	log.Infof(ctx, "container %q cpus ids before applying shared cpus %q", c.Name(), isolatedCPUs.String())
	if err != nil {
		return err
	}
	ctrCpuSet := isolatedCPUs.Union(*sharedCPUs)
	quota, err := calculateCFSQuota(&ctrCpuSet, int64(*(lspec.Resources.CPU.Period)))
	if err != nil {
		return err
	}
	pid, err := c.Pid()
	if err != nil {
		return fmt.Errorf("failed to get pid of container %s: %w", c.ID(), err)
	}
	ch, err := node.CgroupBuildHierarchyFrom(pid)
	if err != nil {
		return fmt.Errorf("failed to build cgroup hierarchy of container %s: %w", c.ID(), err)
	}

	podCpusetCgroup := ch.GetAbsoluteControllerPodPath("cpuset")
	podCpuAcctCgroup := ch.GetAbsoluteControllerPodPath("cpuacct")
	// pod level operations
	_, err = addOrRemoveCpusetFromFile(podCpusetCgroup, cpusetCpus, *sharedCPUs, true)
	if err != nil {
		return err
	}
	if err = cgroups.WriteFile(podCpuAcctCgroup, quotaFile(), strconv.FormatInt(quota, 10)); err != nil {
		return err
	}

	ctrCpusetCgroup := ch.GetAbsoluteControllerContainerPath("cpuset")
	ctrCpuAcctCgroup := ch.GetAbsoluteControllerContainerPath("cpuacct")

	_, err = addOrRemoveCpusetFromFile(ctrCpusetCgroup, cpusetCpus, *sharedCPUs, true)
	if err != nil {
		return err
	}
	log.Infof(ctx, "container %q cpus ids after applying shared cpus %q", c.Name(), ctrCpuSet.String())

	if err = cgroups.WriteFile(ctrCpuAcctCgroup, quotaFile(), strconv.FormatInt(quota, 10)); err != nil {
		return err
	}
	// we need to move the isolated cpus into a separate child cgroup
	if shouldCPULoadBalancingBeDisabled(s.Annotations()) && node.CgroupIsV2() {
		// on V2 all controllers are under the same path
		ctrCgroup := ctrCpusetCgroup
		if err = cgroups.WriteFile(ctrCgroup, cgroupSubTreeControl, "+cpu +cpuset"); err != nil {
			return err
		}
		if err = cgroups.WriteFile(ctrCgroup, cpusetPartition, "member"); err != nil {
			return err
		}
		cgroupChildDir := filepath.Join(ctrCgroup, "cgroup-child")
		if err = os.Mkdir(cgroupChildDir, 755); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		if err = cgroups.WriteFile(cgroupChildDir, cpusetCpus, isolatedCPUs.String()); err != nil {
			return err
		}
		if err = cgroups.WriteFile(cgroupChildDir, cpusetExclusive, isolatedCPUs.String()); err != nil {
			return err
		}
		if err = cgroups.WriteFile(cgroupChildDir, cpusetPartition, "isolated"); err != nil {
			return err
		}
	}
	injectCpusetEnv(c, &isolatedCPUs, sharedCPUs)
	return nil
}

func calculateCFSQuota(cpus *cpuset.CPUSet, period int64) (quota int64, err error) {
	quan, err := resource.ParseQuantity(strconv.Itoa(cpus.Size()))
	if err != nil {
		return
	}
	quota = (quan.MilliValue() * period) / milliCPUToCPU
	return
}

func quotaFile() string {
	if node.CgroupIsV2() {
		return cgroupV2Quota
	}
	return cgroupV1Quota
}

func injectCpusetEnv(c *oci.Container, isolated, shared *cpuset.CPUSet) {
	spec := c.Spec()
	spec.Process.Env = append(spec.Process.Env,
		fmt.Sprintf("%s=%s", IsolatedCPUsEnvVar, isolated.String()),
		fmt.Sprintf("%s=%s", SharedCPUsEnvVar, shared.String()))
	c.SetSpec(&spec)
}
