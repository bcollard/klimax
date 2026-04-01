SLIRP = Simple Layer Implementation for Receiving Packets — it's user-mode networking for the VM.

  In Lima's context:
  - Provides outbound internet connectivity via userspace NAT (no root/kernel modules required)
  - Implemented via gvisor-tap-vsock
  - Hardcoded subnet 192.168.5.0/24, guest gets 192.168.5.15, host reachable at 192.168.5.2 (host.lima.internal)
  - It's Lima's default network mode

  klimax doesn't use it — you use vzNAT (VZNATNetworkDeviceAttachment) instead, which is Apple's native Virtualization.framework NAT and gives the VM a proper lima0 interface with a macOS-assigned IP on bridge1xx.
  vzNAT is faster and better integrated with macOS than slirp.

  