# Replicating a guest's vTPM state

*NOT IMPLEMENTED. This is in `docs/design/TODO/` because it describes work that has not been done: vmsync detects a vTPM and warns that its state is not replicated, and that is all it does. Every claim about libvirt, swtpm, libtpms and qemu below cites source that was read -- clones of libvirt master and swtpm, plus the pinned `libvirt.org/go/libvirtxml`. The mechanism and the secrets policy were settled by an adversarial review and are recorded as decisions; what remains open is stated as such in place.*

---

## What exists today

`DetectTPM` (`pkg/libvirtsync/libvirt.go`) reads the source's `<tpm>` device and the sync warns once, near the varstore and loader checks in `cmd/vmsync/main.go`. Three cases, because they need different things of a replica:

- **`emulator`** — state is not replicated. The warning says the replica's TPM will be a different, empty one, so anything the guest sealed to the source's TPM has to be re-enrolled there. When the backend carries `<encryption>` it says that too, because that changes what a hand-copy can achieve (below).
- **`passthrough`** — a host TPM device. There is nothing to copy, and the *target host* needs its own TPM for the domain to start at all.
- **`external`** — reported as not understood, which is honest.

Nothing fails on account of a TPM. A replica with a fresh TPM boots; what it cannot do is open sealed secrets. That is the operator's call, and the warning exists so they can make it.

## Why the varstore copy does not already cover this

They are unrelated stores, and conflating them is the obvious first guess:

| | UEFI varstore | emulated TPM state |
| --- | --- | --- |
| XML | `<os><nvram>` | `<tpm><backend type='emulator'>` |
| shape | one file | a **directory** |
| path | e.g. `/var/lib/libvirt/qemu/nvram/<name>_VARS.fd`, derived from the domain **name** | `<swtpmStorageDir>/<uuid>/{tpm2\|tpm1.2}`, derived from the domain **UUID** |
| holds | UEFI boot entries, enrolled Secure Boot keys | endorsement key, storage root key, PCRs, NV indices, sealed blobs |
| vmsync | copied — `cmd/vmsync/nvram.go` | nothing |

The path comes from `qemuTPMEmulatorStorageBuildPath` (`src/qemu/qemu_tpm.c`), which builds `<swtpmStorageDir>/<uuidstr>/<dir>` with `dir` being `tpm2` or `tpm1.2`; `swtpmStorageDir` defaults to `/var/lib/libvirt/swtpm` for the system instance (`src/qemu/qemu_conf.c`).

**One thing is already in our favour.** The path is keyed by domain UUID, and vmsync deliberately gives the replica the **source's** UUID — see `planDefine` in `pkg/libvirtsync/libvirt.go`, where keeping it is the reason a UUID mismatch takes the destructive define path. So the target's state directory would be the *same* `<uuid>` string. No translation, no name-derived rewrite of the kind `TargetNvramPath` has to do for the varstore.

## The four obstacles

### 1. Quiescence — answered: copy one file, not the directory

The obstacle largely dissolves, but only for the right file, and the answer changes the shape of the feature.

**swtpm's directory backend writes every state file through a temp file plus `rename(2)`** (`swtpm/src/swtpm/swtpm_nvstore_dir.c`). The one exception is the permanent-state file when the `backup` suboption is on, and libvirt never passes `backup` — it passes `dir=<path>,mode=0600,lock` only (`qemuTPMVirCommandSwtpmAddTPMState`). So an open-by-path read of `tpm2-00.permall` to EOF always yields **one complete generation** — the old inode or the new one, never a mix. **Tearing is not possible.** What remains is staleness, which is the same class of inexactness the disks already carry.

So: **replicate exactly `tpm2-00.permall`** (`tpm-00.permall` for TPM 1.2), and make the other names *absent* on the replica.

| file | what to do | why |
| --- | --- | --- |
| `tpm2-00.permall` | **copy** | the persistent NV state: hierarchy seeds, EK, SRK, owner/endorsement/lockout auth, NV indices, persistent handles |
| `tpm2-00.volatilestate` | **delete on the target** | poison. libtpms enters *failure mode* if the load fails, and if it succeeds the replica resumes another machine's mid-run PCRs and session state. A cold-booting guest under libvirt never has one — and **nothing deletes a stray**: qemu sets `PTM_INIT_FLAG_DELETE_VOLATILE` only when resuming |
| `tpm2-00.savestate` | delete as hygiene | for TPM 2 under libtpms **this file does not exist**; the suspend data rides inside `permall` (below) |
| `.lock`, `TMP2-00.*` | never copy | a live `fcntl` write lock, and crash leftovers |

**Two assumptions this rests on, which must not be laundered into properties of the mechanism:**

- **Suspend state can ride along inside `permall`.** `PERSISTENT_ALL_Marshal` writes `STATE_RESET_DATA` and `STATE_CLEAR_DATA` — where `pcrSave` lives — whenever `(orderlyState & TPM_SU_STATE_MASK) == TPM_SU_STATE`, i.e. after the guest did `TPM2_Shutdown(STATE)`: Windows sleep, hibernate, or a clean shutdown. Selecting `permall` does **not** by itself keep foreign PCRs out. What clears them is the replica's firmware issuing `TPM2_Startup(CLEAR)` on a cold boot — a property of the guest's boot path that vmsync neither enforces nor verifies. Either refuse a generation written in that state, or say plainly that the replica's cold boot discards it.
- **The re-hash retry is a RECENCY filter, not an integrity check.** Once atomicity is granted the bytes already are one complete generation, so re-hashing can only discard a valid, slightly older one. `nvram.go` justifies its convergence from *rarity* — "the file is quiescent for weeks", "a file that changes a few times a year" — and caps at `nvramCopyAttempts = 3` before erroring. NV commits on a live Windows guest are not rare in that sense, so keeping the retry risks three failures and **no TPM copy at all**. Pick one: keep it and accept that failure, or drop it, accept the generation that was read, and record its timestamp.

**Rejected: asking swtpm for a quiesced export.** The mechanism exists and is unreachable. qemu holds swtpm's control-channel connection for the entire life of the guest (`tpm_emulator.c`: `qemu_chr_fe_init` at startup, `qemu_chr_fe_deinit` only at finalize), and swtpm accepts **one** control client at a time — it stops polling the listening socket while one is connected (`swtpm/src/swtpm/mainloop.c`). A `swtpm_ioctl --save` against a live guest's socket connects into the backlog and blocks indefinitely. **Any design that reaches for the control channel on a running guest is a hang, not a feature.**

**A verification primitive that is safe to use.** `swtpm socket --tpm2 --tpmstate dir=<path> --print-states` is a pure `stat()` of the three names, printing JSON with sizes; it takes no lock, starts no TPM and needs no key (`SWTPM_NVRAM_PrintJson`). Safe on a live source *and* on the target. Pair it with a per-file sha256 as `nvram.go` does, because swtpm's own on-disk integrity is thin: the blob header carries no magic and no checksum, only `totlen == filesize` and `hdrsize == 8`, so truncation is caught and same-length corruption of unencrypted state is not. **Do not use `--print-info` as a probe** — it calls `tpmlib_start(..., storage_locked=true, ...)`, taking the lock and starting a TPM against the state.

### 2. Encryption, and whose key it is

libvirt supports `<backend type='emulator'><encryption secret='UUID'/>`, which it passes to swtpm as `--key` (`src/qemu/qemu_tpm.c`). The secret lives in libvirt **on the source host**.

So for an encrypted vTPM, copying the state directory achieves nothing on its own: the target cannot read it until the same secret exists there. `DetectTPM` reports this case and the warning says so, precisely so nobody wastes an afternoon on a manual `scp`.

**There is a present-day bug here that is bigger than the missing feature, and it is not about replication.** The XML rewrite patches a parsed tree rather than round-tripping — deliberately, so unmodelled content is not silently lost (`warnIfXMLElementsDropped`'s own comment names TPM as an example of what a round-trip drops). So `<backend type='emulator'><encryption secret='UUID'/>` reaches the replica's definition **verbatim**, and libvirt resolves that UUID at **start** time, not at define time. Consequence: on a target with no secret of that UUID, the replica of an encrypted-vTPM domain **does not start at all** — `virDomainCreate` fails — and the operator discovers it at failover. That is true today, with zero state copying, and it is the worst thing in this area.

The sync's warning now says exactly that for the encrypted case rather than the softer "empty TPM" line. **Still to do:** a target-side preflight next to the existing varstore existence check — look up a secret with that UUID and usage type `vtpm` on the target and warn, naming the UUID, when it is absent. It is not built yet because no target libvirt manager is in scope at that point in `run()`; it needs either moving the check later or passing one in. Use `virSecretLookupByUUIDString` / `virsh secret-dumpxml` — **never** `secret-get-value`.

**No export path avoids this.** `swtpm_ioctl --save` asks for the decrypted form, but `GetStateBlob` re-encrypts under the **migration key** whenever one is set, and libvirt sets the migration key from the same secret as the file key (`qemuTPMVirCommandSwtpmAddEncryption`). So every route out of an encrypted source yields ciphertext requiring that secret on the target. There is no clever way round it.

**DECIDED: for encrypted state the answer is refuse, permanently.** Not a backlog item — a decision, on three independent grounds:

- the key's at-rest form on the source is `/etc/libvirt/secrets/<uuid>.base64` — plaintext base64, mode 0600, with at-rest encryption off by default;
- a vTPM secret should be `private='yes'`, and libvirt deliberately refuses `virSecretGetValue` on a private secret;
- copying it is not replicating *state*, it is replicating a **credential whose entire purpose is to be host-local**. A tool whose job is to move disk bytes must not acquire a code path that reads a passphrase out of one host and installs it on another.

So encrypted vTPM state is **pre-provision or nothing**: the operator provisions the same secret UUID and value on the target out of band, through whatever they use for secrets, and vmsync verifies only its **existence and usage type** — never its value.

For the unencrypted case, replicate the state file per the mechanism above. `TPMInfo.EncryptionSecret` carries the UUID rather than a boolean precisely so the refusal and the warning can name it.

### 3. Ownership and labelling — depends entirely on `<source type=>`

**For the default and `type='dir'`, libvirt repairs most of this on every domain start**, not just at creation: `qemuTPMEmulatorCreateStorage` runs unconditionally, calling `virDirCreate(path, 0700, swtpm_user, swtpm_group, ALLOW_EXIST)` — which chowns and chmods an *existing* directory — then `virFileChownFiles` chowns every regular file in it. SELinux labels are likewise reapplied at start by `virSecuritySELinuxSetTPMLabels`. Two residues for vmsync:

- `virFileChownFiles` does `lchown` only and never touches **mode**, so the copied file needs `chmod 0600` itself.
- The copy must **not preserve xattrs**. `trusted.libvirt.security.{dac,selinux,ref_*,timestamp_*}` are libvirt's remembered-owner refcounts; carrying the source's across is noise at best and can suppress relabelling at worst.

**For `<source type='file'>`, none of that repair happens.** libvirt sets `create_storage = false; run_setup = true;` for that shape, so `qemuTPMEmulatorCreateStorage` — and with it the chown and chmod — never runs, and `swtpm_setup` runs on every start instead. A state file written by root stays root-owned, swtpm runs as the configured swtpm user with cleared capabilities, and **the domain fails to start**. This is the case the blunt version of this warning was right about.

**And AppArmor is a prerequisite, not a footnote.** `security_apparmor.c` has no TPM handling at all; `virt-aa-helper` **hardcodes** the rules as `LOCALSTATEDIR/lib/libvirt/swtpm/<uuid>/<tpm2|tpm1.2>/` and never reads `source_path`. So on Debian and Ubuntu a state file at a path the domain names itself gets no generated rule — and that is the same host family where the swtpm user differs from Red Hat's. Any design that recommends the single-file backend has to answer this first.

`DetectTPM` reports `SourceType` and `SourcePath` precisely so this distinction is visible at sync time rather than discovered on the target.

**Two implementation notes that look like reuse and are not.**

- `util.ParseQemuConfOwner` **cannot** be reused to find `swtpm_user`/`swtpm_group`: its regex matches `user|group` only (`pkg/util/diskowner.go`). A separate parse is needed, and the fallback differs in kind — for disks an all-commented file yields empty and the caller warns, whereas libvirt has a compiled default for swtpm with a **silent root fallback** when no `tss` user exists. So "empty means say nothing" is the wrong default to inherit here.
- **libvirt may write to state we pre-placed, before the guest ever runs.** `qemuTPMEmulatorReconfigure` can rewrite it at start — and it needs the encryption secret to do so. The trigger is narrower than it first appears: it returns early unless the version is 2.0 **and** the PCR-bank bitmap string is non-empty **and** `swtpm_setup` advertises `CMDARG_RECONFIGURE_PCR_BANKS`. A design that says "whenever `<active_pcr_banks>` is present" will mispredict on hosts with older `swtpm_setup`.

### 4. PCRs, and what this feature cannot promise

This is the one most likely to be misread as solved. For a sealed volume (BitLocker, and anything else using measured boot) to open on the replica, **two** things must hold: the TPM state must match, *and* the PCR values at boot must match what the volume was sealed against. PCRs measure the firmware and the boot path — so the varstore, the target host's own OVMF build, and the disk contents all have to line up.

Copying TPM state is therefore **necessary but not sufficient**. A design document that implies "replicate the state and BitLocker just works" would be wrong, and the warning text deliberately says "until it is re-enrolled there" rather than promising otherwise.

## Identity: a cloned TPM is a cloned TPM

A TPM is designed to be unclonable, and copying its state gives two machines the same endorsement and storage root keys. For a DR replica that only ever runs when the source is dead, that is the intended outcome — and it is the same trade vmsync already makes with the domain UUID, deliberately.

It is still worth stating in the operator documentation rather than only here, because attestation and device-identity services can notice. **OPEN** as part of the policy decision above: whether anything beyond documentation is warranted.

## Scope when it is built

- The warning becomes conditional on the feature being off, or on the state not having been replicated this run.
- `-verify` says nothing about TPM state and should not pretend to; whatever is copied needs its own integrity statement, as the varstore has its sha256.
- Bench coverage: a stage that plants known TPM state on a source, syncs, and checks the replica's directory matches — which requires the quiescence question answered first, because the stage cannot be more consistent than the mechanism.
- `KEEP_TPM` is **already fixed** and is not part of this. That change stops vmsync *destroying* the target's own swtpm state on undefine; it does not replicate the source's. The two are separate wrongs and only one of them is still open.

## References

- `pkg/libvirtsync/libvirt.go` — `DetectTPM`, `TPMInfo`, and the `KEEP_TPM` rationale in `DefineDomain`
- `cmd/vmsync/nvram.go` — the varstore copy, and the closest precedent for how a non-disk artefact gets replicated
- libvirt `src/qemu/qemu_tpm.c` — storage path, the `0711` parent, `--key`, and the delete-on-undefine behaviour `KEEP_TPM` suppresses
- libvirt `src/qemu/qemu_conf.c` — `swtpmStorageDir` defaults
