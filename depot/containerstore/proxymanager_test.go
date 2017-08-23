package containerstore_test

import (
	"encoding/json"

	"code.cloudfoundry.org/executor/depot/containerstore"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagertest"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("ProxyManager", func() {

	var (
		portMapping []containerstore.ProxyPortMapping
		logger      lager.Logger
	)

	BeforeEach(func() {
		logger = lagertest.NewTestLogger("proxymanager")
	})

	Context("CreateProxyConfig", func() {
		var expectedConfig containerstore.ProxyConfig

		Context("with a single port mapping", func() {
			expectedConfigJSON := `
				{
						"listeners": [
								{
												"address": "tcp://0.0.0.0:8443",
												"filters": [{
														"type": "read",
														"name": "0-proxy",
														"config": {
																"stat_prefix": "ingress_tcp",
																"route_config": {
																		"routes": [{
																				"cluster": "0-service-cluster"
																		}]
																}
														}
												}]
								}
						],
						"admin": {
								"access_log_path": "/tmp/admin_access.log",
								"address": "tcp://127.0.0.1:9901"
						},
						"cluster_manager": {
								"clusters": [
										{
												"name": "0-service-cluster",
												"connect_timeout_ms": 250,
												"type": "static",
												"lb_type": "round_robin",
												"hosts": [{
														"url": "tcp://127.0.0.1:8080"
												}]
										}
								]
						}
				}`

			BeforeEach(func() {
				portMapping = []containerstore.ProxyPortMapping{
					containerstore.ProxyPortMapping{
						AppPort:   8080,
						ProxyPort: 8443,
					},
				}
				err := json.Unmarshal([]byte(expectedConfigJSON), &expectedConfig)
				Expect(err).NotTo(HaveOccurred())
			})

			It("creates the appropriate proxy file", func() {
				config := containerstore.GenerateProxyConfig(logger, portMapping)
				Expect(config).To(Equal(expectedConfig))
			})
		})

		Context("with multiple port mappings", func() {
			expectedConfigJSON := `
			{
					"listeners": [
							{
											"address": "tcp://0.0.0.0:8443",
											"filters": [{
													"type": "read",
													"name": "0-proxy",
													"config": {
															"stat_prefix": "ingress_tcp",
															"route_config": {
																	"routes": [{
																			"cluster": "0-service-cluster"
																	}]
															}
													}
											}]
							},
							{
											"address": "tcp://0.0.0.0:9000",
											"filters": [{
													"type": "read",
													"name": "1-proxy",
													"config": {
															"stat_prefix": "ingress_tcp",
															"route_config": {
																	"routes": [{
																			"cluster": "1-service-cluster"
																	}]
															}
													}
											}]
							}
					],
					"admin": {
							"access_log_path": "/tmp/admin_access.log",
							"address": "tcp://127.0.0.1:9901"
					},
					"cluster_manager": {
							"clusters": [
									{
											"name": "0-service-cluster",
											"connect_timeout_ms": 250,
											"type": "static",
											"lb_type": "round_robin",
											"hosts": [{
													"url": "tcp://127.0.0.1:8080"
											}]
									},
									{
											"name": "1-service-cluster",
											"connect_timeout_ms": 250,
											"type": "static",
											"lb_type": "round_robin",
											"hosts": [{
													"url": "tcp://127.0.0.1:2222"
											}]
									}
							]
					}
			}`

			BeforeEach(func() {
				portMapping = []containerstore.ProxyPortMapping{
					containerstore.ProxyPortMapping{
						AppPort:   8080,
						ProxyPort: 8443,
					},
					containerstore.ProxyPortMapping{
						AppPort:   2222,
						ProxyPort: 9000,
					},
				}

				err := json.Unmarshal([]byte(expectedConfigJSON), &expectedConfig)
				Expect(err).NotTo(HaveOccurred())
			})

			It("creates the appropriate proxy file", func() {
				config := containerstore.GenerateProxyConfig(logger, portMapping)
				Expect(config).To(Equal(expectedConfig))
			})
		})
	})
})
