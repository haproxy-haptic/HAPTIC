# Rejected resource changes

HAPTIC validates watched resource changes before using them to update HAProxy. If
one change produces a template error or invalid configuration, HAPTIC keeps that
resource's last validated revision and continues applying independent valid
changes, including endpoint updates. A new invalid resource remains inactive.

An unrelated admission request can succeed while another observed resource is
invalid. HAPTIC retains the last validated revisions of the rejected changes,
keeps all other observed changes, and runs complete configuration validation on
that view with the proposed object. New valid changes remain part of admission
checks even before reconciliation accepts them. A proposal that still fails is
rejected.

## Find and repair a rejected change

1. Run `kubectl describe` on the affected resource and look for the `InputRejected`
   Warning event. It explains the failure and asks you to correct the resource.
   Controller logs also record `Watched resource change rejected`.
2. Inspect `/debug/vars/inputRejections` through the controller's debug interface
   for the current rejected-resource list.
3. Correct the named resource. HAPTIC retries observed changes automatically and
   removes the rejection once complete validation succeeds.

Alert when `haptic_rejected_watched_inputs` is greater than zero; its `resource_type`,
`namespace` and `name` labels name the rejected resource. Successful traffic
and endpoint updates don't mean the rejected change took effect.

## Behavior while a change is rejected

The previous validated behavior remains active. A failed credential rotation keeps
the previous credential; a failed policy update keeps the previous policy. A
deletion that makes the remaining configuration invalid can also be held. Check
the rejection state when you need to confirm a security change has taken effect.

After a controller restart, HAPTIC validates the observed resources again. The
new controller has no retained revision for a rejected resource, so dependent
routes may remain inactive until you correct it. If it can't establish a valid
input view, it reports the failure and leaves the
running HAProxy configuration in place. Global configuration and template errors
still require repair when removing an individual watched object can't resolve
them.
