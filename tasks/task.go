package tasks

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"strconv"
	"strings"

	. "github.com/cloudfoundry/cf-acceptance-tests/cats_suite_helpers"

	"github.com/cloudfoundry-incubator/cf-test-helpers/cf"
	"github.com/cloudfoundry-incubator/cf-test-helpers/helpers"
	"github.com/cloudfoundry-incubator/cf-test-helpers/workflowhelpers"
	"github.com/cloudfoundry/cf-acceptance-tests/helpers/app_helpers"
	"github.com/cloudfoundry/cf-acceptance-tests/helpers/assets"
	"github.com/cloudfoundry/cf-acceptance-tests/helpers/random_name"
	"github.com/cloudfoundry/cf-acceptance-tests/helpers/skip_messages"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gexec"
	"github.com/cloudfoundry/cf-acceptance-tests/helpers/logs"
)

const policyTimeout = "10s"

type Result struct {
	FailureReason string `json:"failure_reason"`
}

type Task struct {
	Guid       string `json:"guid"`
	Command    string `json:"command"`
	Name       string `json:"name"`
	State      string `json:"state"`
	Result     Result `json:"result"`
	SequenceId int    `json:"sequence_id"`
}

type Tasks struct {
	Resources []Task `json:"resources"`
}

type ProxyResponse struct {
	ListenAddresses []string ""
	Port            int
}

type Destination struct {
	IP       string `json:"destination"`
	Port     int    `json:"ports,string,omitempty"`
	Protocol string `json:"protocol"`
}

func getTaskDetails(appName string) []string {
	listCommand := cf.Cf("tasks", appName).Wait(Config.DefaultTimeoutDuration())
	Expect(listCommand).To(Exit(0))
	listOutput := string(listCommand.Out.Contents())
	lines := strings.Split(listOutput, "\n")
	return strings.Fields(lines[4])
}

func getGuid(appGuid string, sequenceId string) string {
	var tasks Tasks
	readCommand := cf.Cf("curl", fmt.Sprintf("/v3/apps/%s/tasks", appGuid), "-X", "GET").Wait(Config.DefaultTimeoutDuration())
	Expect(readCommand).To(Exit(0))
	err := json.Unmarshal(readCommand.Out.Contents(), &tasks)
	Expect(err).NotTo(HaveOccurred())

	var task Task
	for _, task = range tasks.Resources {
		parsedSequenceId, _ := strconv.Atoi(sequenceId)
		if parsedSequenceId == task.SequenceId {
			break
		}
	}
	return task.Guid
}

func getContainerIP(listenAddresses []string) string {
	for _, listenAddr := range listenAddresses {
		if !strings.HasPrefix(listenAddr, "127.0.0.1") {
			return listenAddr
		}
	}

	return ""
}

func createSecurityGroup(allowedDestinations ...Destination) string {
	file, _ := ioutil.TempFile(os.TempDir(), "CATS-sg-rules")
	defer os.Remove(file.Name())
	Expect(json.NewEncoder(file).Encode(allowedDestinations)).To(Succeed())

	rulesPath := file.Name()
	securityGroupName := random_name.CATSRandomName("SG")

	workflowhelpers.AsUser(TestSetup.AdminUserContext(), Config.DefaultTimeoutDuration(), func() {
		Expect(cf.Cf("create-security-group", securityGroupName, rulesPath).Wait(Config.DefaultTimeoutDuration())).To(Exit(0))
	})

	return securityGroupName
}

var _ = TasksDescribe("v3 tasks", func() {
	var (
		appName string
		appGuid string
	)

	BeforeEach(func() {
		if !Config.GetIncludeTasks() {
			Skip(skip_messages.SkipTasksMessage)
		}
		appName = random_name.CATSRandomName("APP")

	})

	Context("tasks lifecycle", func() {
		BeforeEach(func() {
			Expect(cf.Cf("push",
				appName,
				"--no-start",
				"-b", Config.GetBinaryBuildpackName(),
				"-m", DEFAULT_MEMORY_LIMIT,
				"-p", assets.NewAssets().Catnip,
				"-c", "./catnip",
				"-d", Config.GetAppsDomain()).Wait(Config.DefaultTimeoutDuration())).To(Exit(0))
			app_helpers.SetBackend(appName)
			appGuid = app_helpers.GetAppGuid(appName)
			Expect(cf.Cf("start", appName).Wait(Config.CfPushTimeoutDuration())).To(Exit(0))
			Eventually(func() string {
				return helpers.CurlAppRoot(Config, appName)
			}, Config.DefaultTimeoutDuration()).Should(ContainSubstring("Catnip?"))
		})

		AfterEach(func() {
			app_helpers.AppReport(appName, Config.DefaultTimeoutDuration())

			Expect(cf.Cf("delete", appName, "-f", "-r").Wait(Config.DefaultTimeoutDuration())).To(Exit(0))
		})

		It("can successfully create and run a task", func() {
			By("creating the task")
			taskName := "mreow"
			// sleep for enough time to see the task is RUNNING
			sleepTime := math.Min(float64(2), float64(Config.DefaultTimeoutDuration().Seconds()))
			command := fmt.Sprintf("sleep %f", sleepTime)
			lastUsageEventGuid := app_helpers.LastAppUsageEventGuid(TestSetup)
			createCommand := cf.Cf("run-task", appName, command, "--name", taskName).Wait(Config.DefaultTimeoutDuration())
			Expect(createCommand).To(Exit(0))

			taskDetails := getTaskDetails(appName)
			sequenceId := taskDetails[0]
			outputName := taskDetails[1]
			outputState := taskDetails[2]
			ouputCommand := taskDetails[len(taskDetails)-2] + " " + taskDetails[len(taskDetails)-1]

			Expect(ouputCommand).To(Equal(command))
			Expect(outputName).To(Equal(taskName))
			Expect(outputState).To(Or(Equal("RUNNING"), Equal("SUCCEEDED")))

			taskGuid := getGuid(appGuid, sequenceId)

			By("TASK_STARTED AppUsageEvent")
			usageEvents := app_helpers.UsageEventsAfterGuid(TestSetup, lastUsageEventGuid)
			start_event := app_helpers.AppUsageEvent{Entity: app_helpers.Entity{State: "TASK_STARTED", ParentAppGuid: appGuid, ParentAppName: appName, TaskGuid: taskGuid}}
			Expect(app_helpers.UsageEventsInclude(usageEvents, start_event)).To(BeTrue())

			By("successfully running")

			Eventually(func() string {
				taskDetails = getTaskDetails(appName)
				outputName = taskDetails[1]
				outputState = taskDetails[2]
				return outputState
			}, Config.DefaultTimeoutDuration()).Should(Equal("SUCCEEDED"))

			Expect(outputName).To(Equal(taskName))

			By("TASK_STOPPED AppUsageEvent")
			usageEvents = app_helpers.UsageEventsAfterGuid(TestSetup, lastUsageEventGuid)
			stop_event := app_helpers.AppUsageEvent{Entity: app_helpers.Entity{State: "TASK_STOPPED", ParentAppGuid: appGuid, ParentAppName: appName, TaskGuid: taskGuid}}
			Expect(app_helpers.UsageEventsInclude(usageEvents, stop_event)).To(BeTrue())
		})

		Context("When cancelling a task", func() {
			var taskId string
			var taskName string

			BeforeEach(func() {
				command := "sleep 100;"
				taskName = "mreow"
				createCommand := cf.Cf("run-task", appName, command, "--name", taskName).Wait(Config.DefaultTimeoutDuration())
				Expect(createCommand).To(Exit(0))

				taskDetails := getTaskDetails(appName)
				taskId = taskDetails[0]
			})

			It("should show task is in FAILED state", func() {
				terminateCommand := cf.Cf("terminate-task", appName, taskId).Wait(Config.DefaultTimeoutDuration())
				Expect(terminateCommand).To(Exit(0))

				var outputSequenceId, outputName, outputState string
				Eventually(func() string {
					taskDetails := getTaskDetails(appName)
					outputSequenceId = taskDetails[0]
					outputName = taskDetails[1]
					outputState = taskDetails[2]
					return outputState
				}, Config.DefaultTimeoutDuration()).Should(Equal("FAILED"))
				Expect(outputName).To(Equal(taskName))
				taskGuid := getGuid(appGuid, outputSequenceId)

				readCommand := cf.Cf("curl", fmt.Sprintf("/v3/tasks/%s", taskGuid), "-X", "GET").Wait(Config.DefaultTimeoutDuration())
				Expect(readCommand).To(Exit(0))

				var readOutput Task
				err := json.Unmarshal(readCommand.Out.Contents(), &readOutput)
				Expect(err).NotTo(HaveOccurred())
				failureReason := readOutput.Result.FailureReason
				Expect(failureReason).To(Equal("task was cancelled"))
			})
		})
	})

	Context("when associating a task with an app", func() {
		var securityGroupName string
		BeforeEach(func() {
			Expect(cf.Cf(
				"push", appName,
				"--no-start",
				"-b", Config.GetGoBuildpackName(),
				"-m", DEFAULT_MEMORY_LIMIT,
				"-p", assets.NewAssets().Proxy,
				"-d", Config.GetAppsDomain(),
				"-f", assets.NewAssets().Proxy+"/manifest.yml",
			).Wait(Config.CfPushTimeoutDuration())).To(Exit(0))
			appGuid = app_helpers.GetAppGuid(appName)
			Expect(cf.Cf("start", appName).Wait(Config.CfPushTimeoutDuration())).To(Exit(0))
		})

		AfterEach(func() {
			app_helpers.AppReport(appName, Config.DefaultTimeoutDuration())

			Expect(cf.Cf("delete", appName, "-f", "-r").Wait(Config.DefaultTimeoutDuration())).To(Exit(0))
		})

		It("applies the associated app's policies to the task", func(done Done) {
			By("creating the network policy")
			workflowhelpers.AsUser(TestSetup.AdminUserContext(), Config.DefaultTimeoutDuration(), func() {
				Expect(cf.Cf("target", "-o", TestSetup.RegularUserContext().Org, "-s", TestSetup.RegularUserContext().Space).Wait(Config.DefaultTimeoutDuration())).To(Exit(0))
				Expect(string(cf.Cf("network-policies").Wait(Config.DefaultTimeoutDuration()).Out.Contents())).ToNot(ContainSubstring(appName))
				Expect(cf.Cf("add-network-policy", appName, "--destination-app", appName, "--port", "8080", "--protocol", "tcp").Wait(Config.DefaultTimeoutDuration())).To(Exit(0))
				Expect(string(cf.Cf("network-policies").Wait(Config.DefaultTimeoutDuration()).Out.Contents())).To(ContainSubstring(appName))
			})

			By("getting the overlay ip of app")
			curlArgs := appName + "." + Config.GetAppsDomain()
			curl := helpers.Curl(Config, curlArgs).Wait(Config.DefaultTimeoutDuration())
			contents := curl.Out.Contents()

			var proxyResponse ProxyResponse
			Expect(json.Unmarshal(contents, &proxyResponse)).To(Succeed())
			containerIP := getContainerIP(proxyResponse.ListenAddresses)

			By("creating the task")
			taskName := "woof"
			command := `while true; do
if curl --fail "` + containerIP + `:` + strconv.Itoa(proxyResponse.Port) + `" ; then
	exit 0
fi
done;
exit 1`
			createCommand := cf.Cf("run-task", appName, command, "--name", taskName).Wait(Config.DefaultTimeoutDuration())
			Expect(createCommand).To(Exit(0))

			By("successfully running")
			var outputName, outputState string
			Eventually(func() string {
				taskDetails := getTaskDetails(appName)
				outputName = taskDetails[1]
				outputState = taskDetails[2]
				return outputState
			}, policyTimeout).Should(Equal("SUCCEEDED"))
			Expect(outputName).To(Equal(taskName))

			close(done)
		}, 30*60 /* <-- overall spec timeout in seconds */)

		Context("when binding a space-specific ASG", func() {
			AfterEach(func() {
				workflowhelpers.AsUser(TestSetup.AdminUserContext(), Config.DefaultTimeoutDuration(), func() {
					Expect(cf.Cf("unbind-security-group", securityGroupName, TestSetup.RegularUserContext().Org, TestSetup.RegularUserContext().Space).Wait(Config.DefaultTimeoutDuration())).To(Exit(0))
				})
				workflowhelpers.AsUser(TestSetup.AdminUserContext(), Config.DefaultTimeoutDuration(), func() {
					Expect(cf.Cf("delete-security-group", securityGroupName, "-f").Wait(Config.DefaultTimeoutDuration())).To(Exit(0))
				})
			})

			It("applies the associated app's ASGs to the task", func(done Done) {
				By("creating the ASG")
				destSecurityGroup := Destination{
					IP:       Config.GetUnallocatedIPForSecurityGroup(),
					Port:     80,
					Protocol: "tcp",
				}
				securityGroupName = createSecurityGroup(destSecurityGroup)

				By("binding the ASG to the space")
				workflowhelpers.AsUser(TestSetup.AdminUserContext(), Config.DefaultTimeoutDuration(), func() {
					Expect(cf.Cf("bind-security-group", securityGroupName, TestSetup.RegularUserContext().Org, TestSetup.RegularUserContext().Space).Wait(Config.DefaultTimeoutDuration())).To(Exit(0))
				})

				By("restarting the app to apply the ASG")
				Expect(cf.Cf("restart", appName).Wait(Config.CfPushTimeoutDuration())).To(Exit(0))

				By("creating the task")
				taskName := "woof"
				command := `curl --fail --connect-timeout 20 ` + Config.GetUnallocatedIPForSecurityGroup() + `:80`
				createCommand := cf.Cf("run-task", appName, command, "--name", taskName).Wait(Config.DefaultTimeoutDuration())
				Expect(createCommand).To(Exit(0))

				By("testing that external connectivity to a private ip is not refused (but may be unreachable for other reasons)")
				var outputName, outputState string
				Eventually(func() string {
					taskDetails := getTaskDetails(appName)
					outputName = taskDetails[1]
					outputState = taskDetails[2]
					return outputState
				}, Config.DefaultTimeoutDuration()).Should(Equal("FAILED"))
				Expect(outputName).To(Equal(taskName))
				appLogs := logs.Tail(Config.GetUseLogCache(), appName).Wait(Config.DefaultTimeoutDuration())
				Expect(appLogs).To(Exit(0))
				Expect(string(appLogs.Out.Contents())).To(ContainSubstring("Connection timed out"), "ASG configured to allow connection to the private IP but the app is still refused by private ip")

				close(done)
			}, 30*60 /* <-- overall spec timeout in seconds */)
		})
	})
})
