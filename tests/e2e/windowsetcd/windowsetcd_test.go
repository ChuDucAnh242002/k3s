package windowsetcd

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/k3s-io/k3s/tests"
	"github.com/k3s-io/k3s/tests/e2e"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var ci = flag.Bool("ci", false, "running on CI")

// Environment Variables Info:
// E2E_WINDOWS_BOX: Windows Server 2025 Vagrant box (required)
// E2E_WINDOWS_K3S_BIN: path to k3s.exe matching the Linux K3s version (required)
// E2E_RELEASE_VERSION: published K3s release, or skip to use local binaries (required)
// E2E_LINUX_K3S_BIN: path to the Linux K3s binary (required when E2E_RELEASE_VERSION=skip)
// E2E_LINUX_BOX: Linux server box (default: cloud-image/ubuntu-24.04)
// E2E_LIBVIRT_POOL: libvirt storage pool for the VM disks (default: libvirt default)
// E2E_NODE_CPUS: CPUs per VM (default: 2)
// E2E_NODE_MEMORY: memory per VM in MB (default: 4096)

const (
	windowsName = "agent-0"
	serverIP    = "10.10.10.100"
	windowsIP   = "10.10.10.101"
	token       = "vagrant"
)

var serverNames = []string{"server-0", "server-1", "server-2"}

func Test_E2EWindowsEtcd(t *testing.T) {
	RegisterFailHandler(Fail)
	flag.Parse()
	suiteConfig, reporterConfig := GinkgoConfiguration()
	RunSpecs(t, "Windows Etcd Test Suite", suiteConfig, reporterConfig)
}

var kubeconfig string

// runOnWindows runs a PowerShell command over WinRM; RunCmdOnNode can't be used as it wraps in sudo.
func runOnWindows(cmd string) (string, error) {
	quoted := "'" + strings.ReplaceAll(cmd, "'", `'\''`) + "'"
	out, err := tests.RunCommand(fmt.Sprintf(`vagrant winrm --no-tty %s --shell powershell --command %s`, windowsName, quoted))
	if err != nil {
		return out, fmt.Errorf("failed to run command %q on %s: %s, %v", cmd, windowsName, out, err)
	}
	return out, nil
}

func vagrantUp(node string) error {
	var envs string
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "E2E_") {
			envs += " " + env
		}
	}
	cmd := fmt.Sprintf(`%s vagrant up --no-tty %s &>> vagrant.log`, envs, node)
	fmt.Println(cmd)
	_, err := tests.RunCommand(cmd)
	return err
}

// startWindowsAgent runs k3s.exe as a scheduled task, as a process started from the WinRM session would hang it and die with it.
func startWindowsAgent() error {
	cmd := fmt.Sprintf(`$a = New-ScheduledTaskAction -Execute "cmd.exe" -Argument "/c C:\k3s\k3s.exe agent --server https://%s:6443 --token %s --node-ip %s --flannel-iface k3s > C:\k3s\agent.log 2>&1"; `+
		`$p = New-ScheduledTaskPrincipal -UserId "SYSTEM" -RunLevel Highest; `+
		`$s = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1); `+
		`Register-ScheduledTask -TaskName k3s-agent -Action $a -Principal $p -Settings $s -Force | Out-Null; `+
		`Start-ScheduledTask -TaskName k3s-agent`,
		serverIP, token, windowsIP)
	_, err := runOnWindows(cmd)
	return err
}

// genKubeconfig is used instead of e2e.GenKubeconfigFile, which assumes an eth1 interface and relies on vagrant scp.
func genKubeconfig() (string, error) {
	file := "kubeconfig-" + serverNames[0]
	out, err := tests.RunCommand(fmt.Sprintf("vagrant ssh %s -c 'cat /etc/rancher/k3s/k3s.yaml' 2>/dev/null", serverNames[0]))
	if err != nil {
		return "", err
	}
	kc := regexp.MustCompile(`(?m)==> vagrant:.*\n`).ReplaceAllString(out, "")
	kc = strings.Replace(kc, "127.0.0.1", serverIP, 1)
	return file, os.WriteFile(file, []byte(kc), 0644)
}

var _ = ReportAfterEach(e2e.GenReport)

var _ = Describe("Windows agent joins a multi-server etcd cluster", Ordered, func() {
	It("Starts the first Linux server with embedded etcd", func() {
		Expect(vagrantUp(serverNames[0])).To(Succeed(), e2e.GetVagrantLog(nil))
		var err error
		kubeconfig, err = genKubeconfig()
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			return tests.NodesReady(kubeconfig, serverNames[:1])
		}, "300s", "5s").Should(Succeed())
	})

	It("Joins the other Linux servers to the etcd cluster", func() {
		for _, name := range serverNames[1:] {
			Expect(vagrantUp(name)).To(Succeed(), e2e.GetVagrantLog(nil))
		}
		Eventually(func() error {
			return tests.NodesReady(kubeconfig, serverNames)
		}, "600s", "10s").Should(Succeed())
		e2e.DumpNodes(kubeconfig)
	})

	It("Has every server as an etcd and control-plane member", func() {
		for _, label := range []string{"node-role.kubernetes.io/etcd", "node-role.kubernetes.io/control-plane"} {
			res, err := tests.RunCommand(fmt.Sprintf("kubectl get nodes -l %s -o name --kubeconfig=%s", label, kubeconfig))
			Expect(err).NotTo(HaveOccurred())
			for _, name := range serverNames {
				Expect(res).To(ContainSubstring(name), "label %s", label)
			}
		}
	})

	It("Starts the Windows VM and joins it as an agent", func() {
		Expect(vagrantUp(windowsName)).To(Succeed(), e2e.GetVagrantLog(nil))
		Expect(startWindowsAgent()).To(Succeed())
	})

	It("Registers a Ready Windows node", func() {
		Eventually(func() error {
			return tests.NodesReady(kubeconfig, append([]string{windowsName}, serverNames...))
		}, "900s", "10s").Should(Succeed())
		e2e.DumpNodes(kubeconfig)

		res, err := tests.RunCommand("kubectl get nodes -l kubernetes.io/os=windows -o name --kubeconfig=" + kubeconfig)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(ContainSubstring(windowsName))
	})

	It("Runs a pod on the Windows node", func() {
		_, err := tests.RunCommand("kubectl apply -f ../amd64_resource_files/windows-workload.yaml --kubeconfig=" + kubeconfig)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			cmd := "kubectl get pods -l app=windows-test --field-selector=status.phase=Running -o=name --kubeconfig=" + kubeconfig
			res, err := tests.RunCommand(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res).To(ContainSubstring("windows-test"))
		}, "1800s", "10s").Should(Succeed())
	})
})

var failed bool
var _ = AfterEach(func() {
	failed = failed || CurrentSpecReport().Failed()
})

var _ = AfterSuite(func() {
	if failed {
		var servers []e2e.VagrantNode
		for _, name := range serverNames {
			servers = append(servers, e2e.VagrantNode{Name: name})
		}
		AddReportEntry("server-journal", e2e.TailJournalLogs(1000, servers))
		if out, err := tests.RunCommand("kubectl describe pods --kubeconfig=" + kubeconfig); err == nil {
			AddReportEntry("pod-descriptions", out)
		}
		if out, err := runOnWindows(`Get-Content C:\k3s\agent.log -Tail 200`); err == nil {
			AddReportEntry("windows-agent-log", out)
		}
	}
	if !failed || *ci {
		Expect(e2e.DestroyCluster()).To(Succeed())
		os.Remove(kubeconfig)
	}
})
