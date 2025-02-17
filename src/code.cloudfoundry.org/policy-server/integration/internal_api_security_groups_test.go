package integration_test

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"code.cloudfoundry.org/cf-networking-helpers/db"
	"code.cloudfoundry.org/cf-networking-helpers/testsupport"
	"code.cloudfoundry.org/cf-networking-helpers/testsupport/metrics"
	"code.cloudfoundry.org/cf-networking-helpers/testsupport/ports"
	locketconfig "code.cloudfoundry.org/locket/cmd/locket/config"
	"code.cloudfoundry.org/locket/cmd/locket/testrunner"
	lockettestrunner "code.cloudfoundry.org/locket/cmd/locket/testrunner"
	"code.cloudfoundry.org/policy-server/api"
	"code.cloudfoundry.org/policy-server/cc_client"
	"code.cloudfoundry.org/policy-server/config"
	"code.cloudfoundry.org/policy-server/integration/helpers"
	testhelpers "code.cloudfoundry.org/test-helpers"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"github.com/tedsuo/ifrit"
	ginkgomon "github.com/tedsuo/ifrit/ginkgomon_v2"
)

var _ = Describe("Internal API Listing security groups", func() {
	var (
		sessions                  []*gexec.Session
		asgSyncerSession          *gexec.Session
		asgSyncerConfig           config.ASGSyncerConfig
		tlsConfig                 *tls.Config
		policyServerConfs         []config.Config
		policyServerInternalConfs []config.InternalConfig
		internalConf              config.InternalConfig
		dbConf                    db.Config
		locketDBConf              db.Config
		locketProcess             ifrit.Process

		fakeMetron   metrics.FakeMetron
		mockCCServer *helpers.ConfigurableMockCCServer
	)

	BeforeEach(func() {
		fakeMetron = metrics.NewFakeMetron()

		dbConf = testsupport.GetDBConfig()
		dbConf.DatabaseName = fmt.Sprintf("internal_api_security_groups_index_test_node_%d", ports.PickAPort())

		mockCCServer = helpers.NewConfigurableMockCCServer()
		mockCCServer.Start()

		mockCCServer.AddSecurityGroup(cc_client.SecurityGroupResource{
			GUID:  "sg-guid-1",
			Name:  "sg-name-1",
			Rules: []cc_client.SecurityGroupRule{{Description: "sg-rule-1", Protocol: "tcp"}},
			GloballyEnabled: cc_client.SecurityGroupGloballyEnabled{
				Staging: true,
				Running: false,
			},
			Relationships: cc_client.SecurityGroupRelationships{
				StagingSpaces: cc_client.SecurityGroupSpaceRelationship{Data: []map[string]string{{"guid": "space-a"}}},
				RunningSpaces: cc_client.SecurityGroupSpaceRelationship{Data: []map[string]string{{"guid": "space-b"}}},
			},
		})

		mockCCServer.AddSecurityGroup(cc_client.SecurityGroupResource{
			GUID:  "sg-guid-2",
			Name:  "sg-name-2",
			Rules: []cc_client.SecurityGroupRule{{Description: "sg-rule-2"}},
			GloballyEnabled: cc_client.SecurityGroupGloballyEnabled{
				Staging: false,
				Running: false,
			},
			Relationships: cc_client.SecurityGroupRelationships{
				StagingSpaces: cc_client.SecurityGroupSpaceRelationship{Data: []map[string]string{{"guid": "space-c"}}},
				RunningSpaces: cc_client.SecurityGroupSpaceRelationship{Data: []map[string]string{{"guid": "space-c"}}},
			},
		})

		mockCCServer.AddSecurityGroup(cc_client.SecurityGroupResource{
			GUID:  "sg-guid-3",
			Name:  "sg-name-3",
			Rules: []cc_client.SecurityGroupRule{{Description: "sg-rule-3", Ports: "8080"}},
			GloballyEnabled: cc_client.SecurityGroupGloballyEnabled{
				Staging: false,
				Running: false,
			},
			Relationships: cc_client.SecurityGroupRelationships{
				StagingSpaces: cc_client.SecurityGroupSpaceRelationship{Data: []map[string]string{{"guid": "space-a"}}},
				RunningSpaces: cc_client.SecurityGroupSpaceRelationship{Data: []map[string]string{{"guid": "space-a"}}},
			},
		})

		locketPort := ports.PickAPort()
		locketAddress := fmt.Sprintf("127.0.0.1:%d", locketPort)
		locketBinaryPath, err := gexec.Build("code.cloudfoundry.org/locket/cmd/locket", "-race")
		Expect(err).NotTo(HaveOccurred())
		locketDBConf = testsupport.GetDBConfig()
		locketDBConf.DatabaseName = fmt.Sprintf("internal_api_security_groups_index_test_locket_%d", locketPort)
		locketDBConnectionString, err := locketDBConf.ConnectionString()
		Expect(err).NotTo(HaveOccurred())
		testhelpers.CreateDatabase(locketDBConf)

		locketRunner := lockettestrunner.NewLocketRunner(locketBinaryPath, func(cfg *locketconfig.LocketConfig) {
			cfg.ListenAddress = locketAddress
			cfg.DatabaseDriver = dbConf.Type
			cfg.DatabaseConnectionString = locketDBConnectionString
		})

		locketProcess = ifrit.Invoke(locketRunner)
		Eventually(locketProcess.Ready()).Should(BeClosed())

		var defaultExternalConf config.Config
		var defaultInternalConf config.InternalConfig
		defaultExternalConf, defaultInternalConf, asgSyncerConfig = helpers.DefaultTestConfigWithCCServer(dbConf, fakeMetron.Address(), "fixtures", mockCCServer.URL())
		asgSyncerConfig.LocketAddress = locketAddress
		locketClientConfig := testrunner.ClientLocketConfig()
		asgSyncerConfig.LocketCACertFile = locketClientConfig.LocketCACertFile
		asgSyncerConfig.LocketClientCertFile = locketClientConfig.LocketClientCertFile
		asgSyncerConfig.LocketClientKeyFile = locketClientConfig.LocketClientKeyFile
		policyServerConfs = configurePolicyServers(defaultExternalConf, 1)
		policyServerInternalConfs = configureInternalPolicyServers(defaultInternalConf, 1)
		internalConf = policyServerInternalConfs[0]

		sessions = startPolicyAndInternalServers(policyServerConfs, policyServerInternalConfs)

		asgSyncerSession = helpers.StartAsgSyncer(policyServerAsgSyncerPath, asgSyncerConfig)

		tlsConfig = helpers.DefaultTLSConfig()
	})

	AfterEach(func() {
		asgSyncerSession.Interrupt()
		Eventually(asgSyncerSession, helpers.DEFAULT_TIMEOUT).Should(gexec.Exit())
		stopPolicyServerExternalAndInternal(sessions, policyServerConfs, policyServerInternalConfs)
		mockCCServer.Close()
		fakeMetron.Close()
		ginkgomon.Interrupt(locketProcess, 5*time.Second)
		testhelpers.RemoveDatabase(locketDBConf)
	})

	Describe("listing security groups", func() {
		BeforeEach(func() {
			// wait for ASG Syncer to get security groups from cloud controller
			time.Sleep(time.Duration(asgSyncerConfig.ASGSyncInterval) * time.Second * 2)
		})

		listSecurityGroups := func(queryString string, expectedResponse api.AsgsPayload) {
			resp := helpers.MakeAndDoHTTPSRequest(
				"GET",
				fmt.Sprintf("https://%s:%d/networking/v1/internal/security_groups%s", internalConf.ListenHost, internalConf.InternalListenPort, queryString),
				nil,
				tlsConfig,
			)

			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			responseString, err := ioutil.ReadAll(resp.Body)
			Expect(err).NotTo(HaveOccurred())

			var responseJson api.AsgsPayload
			err = json.Unmarshal(responseString, &responseJson)
			Expect(err).NotTo(HaveOccurred())
			Expect(responseJson.Next).To(Equal(expectedResponse.Next))
			Expect(responseJson.SecurityGroups).To(ConsistOf(expectedResponse.SecurityGroups))

			Eventually(fakeMetron.AllEvents, "5s").Should(ContainElement(
				HaveName("InternalSecurityGroupsRequestTime"),
			))

			Eventually(fakeMetron.AllEvents, "5s").Should(ContainElement(
				HaveName("SecurityGroupsStoreBySpaceGuidsSuccessTime"),
			))
		}

		sgRule1 := `{"protocol":"tcp","destination":"","ports":"","type":0,"code":0,"description":"sg-rule-1","log":false}`
		sgRule2 := `{"protocol":"","destination":"","ports":"","type":0,"code":0,"description":"sg-rule-2","log":false}`
		sgRule3 := `{"protocol":"","destination":"","ports":"8080","type":0,"code":0,"description":"sg-rule-3","log":false}`
		expectedSecurityGroup1 := api.SecurityGroup{
			Guid:              "sg-guid-1",
			Name:              "sg-name-1",
			Rules:             `[` + sgRule1 + `]`,
			StagingDefault:    true,
			RunningDefault:    false,
			StagingSpaceGuids: []string{"space-a"},
			RunningSpaceGuids: []string{"space-b"},
		}

		expectedSecurityGroup2 := api.SecurityGroup{
			Guid:              "sg-guid-2",
			Name:              "sg-name-2",
			Rules:             `[` + sgRule2 + `]`,
			StagingDefault:    false,
			RunningDefault:    false,
			StagingSpaceGuids: []string{"space-c"},
			RunningSpaceGuids: []string{"space-c"},
		}

		expectedSecurityGroup3 := api.SecurityGroup{
			Guid:              "sg-guid-3",
			Name:              "sg-name-3",
			Rules:             `[` + sgRule3 + `]`,
			StagingDefault:    false,
			RunningDefault:    false,
			StagingSpaceGuids: []string{"space-a"},
			RunningSpaceGuids: []string{"space-a"},
		}
		allResponse := api.AsgsPayload{
			Next:           0,
			SecurityGroups: []api.SecurityGroup{expectedSecurityGroup1, expectedSecurityGroup2, expectedSecurityGroup3},
		}
		globalResponse := api.AsgsPayload{
			Next:           0,
			SecurityGroups: []api.SecurityGroup{expectedSecurityGroup1},
		}
		spaceCResponse := api.AsgsPayload{
			Next:           0,
			SecurityGroups: []api.SecurityGroup{expectedSecurityGroup1, expectedSecurityGroup2},
		}

		DescribeTable("listing security groups", listSecurityGroups,
			Entry("all", "?space_guids=space-a,space-b,space-c", allResponse),
			Entry("filtered spaces", "?space_guids=space-c", spaceCResponse),
			Entry("global", "", globalResponse),
		)

		// cloud controller does not return security groups in specific order
		Describe("pagination", func() {
			It("returns all data with pagination requests", func() {
				var totalResponseSecurityGroups []api.SecurityGroup

				resp := helpers.MakeAndDoHTTPSRequest(
					"GET",
					fmt.Sprintf("https://%s:%d/networking/v1/internal/security_groups?space_guids=space-a,space-b,space-c&limit=2", internalConf.ListenHost, internalConf.InternalListenPort),
					nil,
					tlsConfig,
				)

				Expect(resp.StatusCode).To(Equal(http.StatusOK))
				responseString, err := ioutil.ReadAll(resp.Body)
				Expect(err).NotTo(HaveOccurred())

				var firstResponseJson api.AsgsPayload
				err = json.Unmarshal(responseString, &firstResponseJson)
				Expect(err).NotTo(HaveOccurred())

				Expect(firstResponseJson.Next).To(Equal(3))
				Expect(firstResponseJson.SecurityGroups).To(HaveLen(2))

				totalResponseSecurityGroups = append(totalResponseSecurityGroups, firstResponseJson.SecurityGroups...)

				resp = helpers.MakeAndDoHTTPSRequest(
					"GET",
					fmt.Sprintf("https://%s:%d/networking/v1/internal/security_groups?space_guids=space-a,space-b,space-c&limit=2&from=3", internalConf.ListenHost, internalConf.InternalListenPort),
					nil,
					tlsConfig,
				)

				Expect(resp.StatusCode).To(Equal(http.StatusOK))
				responseString, err = ioutil.ReadAll(resp.Body)
				Expect(err).NotTo(HaveOccurred())

				var secondResponseJson api.AsgsPayload
				err = json.Unmarshal(responseString, &secondResponseJson)
				Expect(err).NotTo(HaveOccurred())

				Expect(secondResponseJson.Next).To(Equal(0))
				Expect(secondResponseJson.SecurityGroups).To(HaveLen(1))

				Expect(firstResponseJson.SecurityGroups).NotTo(ContainElement(secondResponseJson.SecurityGroups[0]))

				totalResponseSecurityGroups = append(totalResponseSecurityGroups, secondResponseJson.SecurityGroups...)

				Expect(totalResponseSecurityGroups).To(ConsistOf(expectedSecurityGroup1, expectedSecurityGroup2, expectedSecurityGroup3))
			})
		})
	})
})
