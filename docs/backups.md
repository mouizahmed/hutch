# Backups and recovery

Without a backup bucket, a machine's boxes exist only on that machine: lose it, and they're gone. With one, Hutch keeps encrypted copies of every box and of the lead's own state, and can bring them back on a new machine. This page covers setting up a bucket, what's in it, and what to do when a machine is lost.

## Add a bucket

Setup offers one; to add it later:

```sh
hutch machine bucket
```

You need an empty S3-compatible bucket and a key that can read, write, list, and delete in it. Leave the endpoint blank for AWS S3, or give `https://host[:port]` for another provider, like Cloudflare R2, MinIO, or Garage. The bucket must support conditional writes (`If-None-Match`), which keep two machines from overwriting each other's snapshots, and conditional deletes (`If-Match`), which garbage collection needs. AWS S3 supports both; Hutch checks before it relies on either.

Several machines need a bucket: workers write their boxes' snapshots to it.

### The recovery passphrase

The form also asks for a recovery passphrase. Everything in the bucket is encrypted under keys derived from the machine's master key, and the passphrase is what unlocks that key from the bucket on a new machine. Without the passphrase, the bucket can't be read by anyone, including you.

Save it somewhere you can reach without the machine and without this computer, like a password manager.

Hutch also saves the bucket's details on this computer, sealed with the passphrase, in `~/.config/hutch/recovery.json`, so a restore asks only for the passphrase. On another computer, a restore asks for the bucket's details too. To save them on a computer that doesn't have them, run `hutch machine bucket` there.

## What's backed up, and when

| What | When | Kept |
| --- | --- | --- |
| Awake, active boxes | Every `checkpoint.interval` (15 minutes), and first after `checkpoint.first_after` (2 minutes) active | `retention.box_checkpoints` (3) per box |
| A box going to sleep | As it sleeps | As above |
| The lead's state: projects, boxes, logins, settings | Every `backup.interval` (1 minute), when it changed | `retention.backups` (30 days), thinning to one per hour, then one per day |
| Template versions | When they're made | As long as the version |

Snapshots are split into chunks, so unchanged data is uploaded once. The most work that losing a machine can lose is the time since a box's last checkpoint. `hutch doctor` warns when the lead's changes have waited more than `backup.warn_after` (5 minutes) to be backed up.

### Garbage collection

Every `gc.interval` (24 hours), the machine deletes old snapshots and the chunks nothing uses any more. Chunks younger than `gc.grace` (24 hours) are never deleted, so a sweep can't race an upload. To run it now:

```sh
hutch machine gc
```

The bucket's credentials are on every machine, so a compromised machine could delete objects. Turn on your provider's object versioning or retention if you need to recover from that.

## A lost machine

For a single machine that's gone, set up a new one from the bucket:

```sh
hutch machine restore <new-ssh-host>
```

The new machine must be fresh. Restore asks for the passphrase, installs Hutch, restores the lead's newest backup, and makes the new machine yours. Boxes come back from their newest snapshots the first time you use each one, so the restore itself is quick. Work since a box's last checkpoint is lost, and so are changes since the last state backup, at most `backup.interval`.

## A lost worker

When a worker stays unreachable for `machines.lost_notify_after` (5 minutes), you're notified. Hutch never moves its boxes on its own, since an unreachable machine might still be running them. When you know it's gone:

```sh
hutch machine rm --lost <machine>
```

This first fences the machine over SSH, restarting its `hutchd` with every box asleep, so it can't keep running its copies. Then its boxes come back on the other machines from their newest snapshots.

If the machine can't be reached over SSH, add `--down`, but only if you know it's off or cut off from the bucket and the other machines: otherwise two copies of each of its boxes would keep working. Every machine writes a heartbeat to the bucket, and `--down` is refused while the machine's heartbeat is recent.

A lost machine that comes back and is added again keeps its old copies asleep as `<box>-returned`, for you to look through or remove.

## A lost lead

With several machines, the boxes on workers keep running when the lead is unreachable. Pointed at a worker, with `HUTCH_MACHINE=<worker>`, the CLI can list, attach to, exec in, forward ports of, sleep, and wake that worker's boxes. Anything else waits for a lead.

If the lead is gone, promote a worker from the backup:

```sh
hutch machine lead <worker-ssh-host> --from-backup
```

This asks for the passphrase, fences the old lead over SSH (or, with `--down`, takes your word that it's off), restores the lead's newest backup on the worker, and makes it the lead. Changes since the last state backup, at most `backup.interval`, are lost; boxes made or moved in that window are recovered from the workers' reports. The old lead's boxes come back elsewhere when used.

Leads are numbered by term in a signed record in the bucket. An old lead that comes back and finds a newer term stops leading, and keeps its old state aside. Once you've checked the new lead, delete that state with `hutch machine forget-stale <ssh-host>`, and add the machine back as a worker with `hutch machine add`.

## Check your backups

```sh
hutch doctor
```

Doctor checks the bucket from each machine, that it supports conditional writes, and when the last state backup was. Garbage collection checks conditional deletes itself, before each sweep. A real test is the only proof: restore onto a spare machine with `hutch machine restore`, wake a box, and check its files, then remove the spare machine. Do that before you depend on it.
