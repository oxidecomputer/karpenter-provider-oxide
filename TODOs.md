# TODOs

1. Wire up Oxide provisioning parameters in the OxideNodeClass Spec
  - ImageID (make sure it wires through to NodeClaim so it's displayed in kubectl)
  - Boot Disk Size
  - Additional Disks?
  - Local Disks?
  - Additional UserData that is merged

2. CPU Platform label requirement

3. Anti-Affinity Groups? 

4. Other Bootstrap providers (ClusterAPI? Omni?)

5. Composite Providers or other setup to run 2 Karpenters