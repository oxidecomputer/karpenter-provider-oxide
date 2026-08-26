# TODOs

1. Wire up Oxide provisioning parameters in the OxideNodeClass Spec
  - ImageID (make sure it wires through to NodeClaim so it's displayed in kubectl)
  - ~~Boot Disk Size~~ (spec.storage.boot.size)
  - ~~Additional Disks~~ (spec.storage.diskSets)
  - ~~Local Disks~~ (spec.storage.diskSets[].type: Local)
  - Additional UserData that is merged
    - Note: disk setup should run before user-supplied userData, so a user script can rely on the
      mounts and on the /dev/karpenter/<set>-<ordinal> names being present.

2. CPU Platform label requirement

3. Anti-Affinity Groups? 

4. Other Bootstrap providers (ClusterAPI? Omni?)

5. Composite Providers or other setup to run 2 Karpenters

6. Ship the node-side disk tooling in the image rather than in user-data

   pkg/bootstrap/disk/scripts/disk-setup.sh is a static file today, embedded and written out by the
   user-data on every launch. It should instead be part of a node image, so user-data carries only
   the per-node data -- the udev rules, the fstab block and disks.conf -- and a call into it. That
   drops a few KiB from every instance's user-data, makes the script versionable and signable with
   the image, and lets it be tested as a package rather than as a string. The split is already in
   place: the script never varies, and everything that does is data beside it.

6. Orphaned disk GC controller

   A launch creates up to 8 disks, and each is created before the instance is. A process killed
   between the disk creates and the instance create leaks them: they are found today only by a Delete
   for that exact NodeClaim, and nothing calls that if the NodeClaim never reached etcd. Every data
   disk is named with a "kd-<fingerprint>-" prefix derived from the instance name, so a periodic
   sweeper is straightforward -- list the project's disks with that prefix, delete those that are
   detached, older than an hour, and have no live NodeClaim. See pkg/providers/storage for the naming.
