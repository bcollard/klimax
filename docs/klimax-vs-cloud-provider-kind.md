
  1. Real IPs — MetalLB gives LoadBalancer services actual routable IPs (172.30.1.16, etc.), not localhost:ephemeral-port. This is much closer to production behavior and lets you test DNS, TLS SNI, multi-port
  services without surprises.
  2. No port-mapping friction — cloud-provider-kind on macOS requires sudo + Envoy containers + port translation. klimax's L3 route means zero NAT between host and cluster.
  3. Stable addresses — MetalLB pools are deterministic per cluster num. cloud-provider-kind assigns ephemeral mapped ports that change on restart.
  4. Self-contained — klimax handles the entire stack (VM, Docker, registries, routing, clusters). cloud-provider-kind is a piece you bolt onto an existing setup.

