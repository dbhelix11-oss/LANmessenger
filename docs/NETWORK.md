# Network topology

lanmessenger works by having **every client dial out to one relay**. As long as
each device can open a TCP connection to the relay's address, it can chat with
every other device — regardless of which router, subnet, or Wi-Fi band it is on.

This document explains why, and how to place the relay so that "regardless of
which router" actually holds.

## Why not just auto-discover peers on the LAN?

Classic LAN messengers find each other with UDP broadcast or multicast/mDNS
("Bonjour"). Those packets **do not cross a router**. If some family members are
on the ISP router's network (say `192.168.0.0/24`) and others are behind a
second router like the OpenWRT box (`192.168.1.0/24`), broadcast-based discovery
silently sees only half the household.

A relay sidesteps this entirely: discovery becomes "connect to `10.0.0.5:8443`",
which is a normal routed TCP connection, not a broadcast.

## Where to put the relay

The rule: **the relay must sit somewhere every client can route to.**

### Case A — one flat network (simplest)

Everything is on one subnet (one router, or a second router running as a dumb
access point / bridge). Put the relay on any always-on machine on that subnet.
Give it a fixed address (DHCP reservation on the router, keyed to its MAC).
Done.

### Case B — OpenWRT downstream of the ISP router (double NAT)

```
Internet ── ISP router (192.168.0.1) ──┬── Mom's iMac      192.168.0.20
                                       │
                                       └── OpenWRT WAN     192.168.0.2
                                           OpenWRT LAN (192.168.1.1) ──┬── Dad's PC   192.168.1.20
                                                                       └── Kid's PC   192.168.1.21
```

Devices on the OpenWRT LAN can reach the ISP-router LAN (that's just "upstream",
normal outbound routing). Devices on the ISP-router LAN **cannot** reach into
the OpenWRT LAN unless you add a port-forward or a static route.

So put the relay on the **ISP-router LAN** (Case A applies to that subnet, and
the OpenWRT-side clients reach it outbound). For example, run it on Mom's iMac
at `192.168.0.20`, or on a Pi plugged into the ISP router.

If the relay has to live on the OpenWRT LAN instead, forward its port on the
OpenWRT router:

```
# OpenWRT: Network → Firewall → Port Forwards
#   External port 8443 (on WAN) → 192.168.1.20:8443
```

…and point ISP-side clients at the OpenWRT router's WAN IP (`192.168.0.2:8443`).
OpenWRT-side clients still use the relay's LAN IP (`192.168.1.20:8443`). Two
addresses for one relay is fine — each client stores whichever one works for it.

### Case C — two sibling subnets, no path between them

```
                 ┌── Router A  (192.168.10.1) ── clients …
Internet ── modem ┤
                 └── Router B  (192.168.20.1) ── clients + relay
```

Router A's clients have no route to Router B's LAN. Fix it one of two ways:

1. **Static route.** On Router A add: "network `192.168.20.0/24` via
   `<Router B's address on the shared upstream>`", and the reverse on Router B if
   needed. Then Case A applies.
2. **Move the relay to the shared upstream** (the modem/gateway segment both
   routers touch), if anything there can run it.

### Case D — reachable from outside the LAN

All three cases above assume every client is on some LAN the relay can
also reach. For a family member away from the house, that's not true, and
the home router has no public IP and no port-forwarding — so instead of
extending the LAN, a small cloud component becomes the relay's remote
front door:

```
 remote family member         AWS box (Tor hidden service)          home Pi
 ─────────────────────         ─────────────────────────           ────────
 lanmsg-remote-cli                                                  lanmsg-server
 (clientcore + SOCKS5)                                              (unmodified relay
        │                                                            logic; [tunnel]
        │ dial <onion>.onion:8443                                   config block
        │ via local Tor SOCKS proxy                                 added to
        ▼                                                           server.toml)
   [ Tor network ]  ──rendezvous, no inbound port on AWS──▶  Tor daemon on AWS
                                                              (torrc: HiddenServiceDir +
                                                               two HiddenServicePort lines)
                                                                     │
                                                     forwards to loopback only:
                                                     virtual :8443 → 127.0.0.1:8443 (public)
                                                     virtual :9443 → 127.0.0.1:9443 (backend)
                                                                     │
                                                          ┌──────────┴──────────┐
                                                          │   lanmsg-tunnel      │
                                                          │  (new, stateless,   │
                                                          │   loopback-only)    │
                                                          └──────────┬──────────┘
                                                                     │ yamux stream per
                                                                     │ remote client
                                                                     ▼
                                                          persistent connection ◀── Pi dials OUT
                                                          (Argon2id+HMAC auth,      via its own
                                                           no extra TLS — Tor       local Tor SOCKS
                                                           already authenticates    proxy
                                                           the .onion endpoint)
```

The Pi's role doesn't change at all — it's still only ever the one making
outbound connections, exactly like Cases A–C — it just gets one more
outbound destination in addition to (not instead of) listening on the
LAN. Full setup walkthrough (provisioning the cloud box, Tor config on
both ends, the `[tunnel]` config block) is in [SETUP.md](SETUP.md). Design
rationale and the exact trust boundary are in
[DESIGN.md §13](DESIGN.md#13-reaching-the-relay-from-outside-the-lan-the-tor-tunneled-cloud-relay).

## Firewall

The relay listens on one TCP port (default `8443`). Allow inbound connections to
it from every LAN subnet your family uses. On OpenWRT that's a traffic rule:

```
config rule
    option name 'Allow-lanmsg'
    option src 'lan'
    option dest_port '8443'
    option target 'ACCEPT'
    option proto 'tcp'
```

The relay speaks TLS on that port and nothing else. No inbound ports are needed
on the clients — they only make outbound connections.

## Give the relay a stable name

Clients store the relay as `host:port`. Use one of:

- A DHCP reservation so its IP never changes, or
- A hostname your router's DNS resolves (e.g. `relay.lan`), configured as a
  static DHCP host entry.

Then every client is configured once with `relay.lan:8443` and keeps working
across reboots and lease renewals.

## What the relay can and cannot see

It sees: who is connected, who messages whom, when, and how big each message is.
It relays and (when a recipient is offline) briefly stores the **ciphertext**.

It cannot see: message text, file contents, or file names — those are encrypted
end-to-end between the two devices with keys the relay never has. See
[DESIGN.md](DESIGN.md) for the details.
