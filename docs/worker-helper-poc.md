# Worker image authentication POC

Workers use their attached GCE service account to read release images from GAR.
Control-plane registry clients keep the original HostedCluster pull secret.
There is no GAR ExternalSecret or token-distribution loop in this worker path.

## Package and delivery

The NodePool controller embeds
[`docker-credential-gcp`](../controllers/nodepool/workerauth/assets/docker-credential-gcp)
and renders one ConfigMap containing a helper MachineConfig and a native
`ImageDigestMirrorSet`. The NodePool references it from creation. HyperShift's
existing Machine Config rendering produces the mirrors and helper configuration
in initial Ignition, before the first bootstrap Podman pull.
No external binary download or RHCOS rebuild is required to install this script.

The script implements Docker credential lookup using the GCE metadata service.
It accepts only configured GAR hosts, bounds retries, validates the response,
and returns credentials without saving them. Its bytes match the earlier tested
helper: SHA256 `256f891e205dd3d3d50e1dc0756bd9250797a67f652cdb1b8e13e55bf710b6e7`.

| Worker consumer | Credential path |
| --- | --- |
| Kubelet, direct GAR references | Shipped `gcr-credential-provider` and existing platform configuration |
| Bootstrap Podman, CRI-O mirror pulls, OS image operations | Embedded Docker helper, using the same VM identity |

Generated authfiles contain empty `auths` and static `credHelpers` mappings.
Routing uses native `ImageDigestMirrorSet` configuration, rendered by OpenShift
into the registry settings used by CRI-O and Podman. Gecko adds no custom image
resolver, TOML generator or registry-client override. The Docker helper supplies
authentication for the actual GAR endpoint.
A guest MutatingAdmissionPolicy could rewrite API-created Pods, but cannot cover
bootstrap Podman or host OS pulls, so this worker path does not require it.

Verify the built-in provider separately; a successful Pod pull alone does not
identify which credential path supplied authentication.

## Dev configuration

Set the NodePool Helm chart's deployment-level values:

```yaml
workerRegistryMirrors:
  quay.io: us-central1-docker.pkg.dev/dev-mgt-us-c1-drejniak/quay-pullthrough
  registry.redhat.io: us-central1-docker.pkg.dev/dev-mgt-us-c1-drejniak/redhat-pullthrough
```

This configures pools reconciled by that controller; it adds no HostedCluster
opt-in or API field. Empty values retain the original worker configuration.
Use only the isolated dev deployment. Its worker VM identity needs GAR reader
access to both repositories and suitable VM OAuth scopes.

Before provisioning, remove the old Gecko GAR/ESO configuration and HyperShift's
GAR registry overrides. Retain original pull-secret delivery and management-cluster
Pod image routing. Deploy the new NodePool controller image and chart together;
other Gecko services and the Platform API do not need an upgrade.

## Acceptance sequence

1. Create a fresh HostedCluster using its original `pull-secret`.
2. Create its NodePool with zero replicas. Audit final rendered Ignition for
   helper installation, mirrors, helper-only authfiles and service paths before
   a VM boots.
3. Scale to one. Verify private bootstrap, the initial Podman pull, OS image
   operations, both helper paths and a Ready node. Confirm no external worker IP
   or Cloud NAT and that Private Google Access is enabled.
4. Pull uncached direct-GAR images and original Quay/Red Hat digest references.
   Repeat after the observed original credential expires; local image cache hits
   do not establish authentication success.
5. Scale up and down. Check fresh bootstrap on each new worker, record kubelet
   and CRI-O identities through expiry, and test metadata-failure recovery.

These are acceptance requirements for this integration, not claims of completed
live validation. Earlier isolated experiments provide prior evidence; repeat
them on the packaged implementation. Metadata and clients can still cache tokens.

## Boundaries and replacement

This isolated POC replaces all worker auth entries and excludes its workers from
Global Pull Secret synchronization. Customer registry credentials and additional
pull secrets need a separate merge/ownership design before production use.
OpenShift owns registry configuration rendering; inspect the final Ignition for
competing mirror policies and the normal OS layer-store configuration. These
NodePool-local mirror policies are rendering input, rather than guest API
objects; they do not appear in guest `oc get idms`. They do not change
HostedCluster image sources or control-plane registry-client routing. Digest
mirror source fallback remains possible, but workers have no upstream
credentials; network restrictions must enforce allowed destinations.

Initial activation or helper/configuration changes alter the NodePool config
hash and cause a normal worker configuration rollout. Runtime token lookup does
not change this configuration or request kubelet restarts. The ConfigMap and
NodePool share a Gecko transport group and are cleaned up together.

When a supported Docker helper ships in the initial OS image, change the static
helper reference and remove this script after validating its protocol, metadata
source, paths and every host consumer. That is a configuration rollout,
independent of routine credential renewal.
