package auth

// SetArgon2HookForTest installs a hook that runs INSIDE the
// semaphore-guarded argon2 region of (*Verifier).Verify, BEFORE the
// real argon2 evaluation loop. Tests use this to:
//
//   - count concurrent in-flight argon2 evaluations to assert the
//     semaphore caps them at runtime.NumCPU(),
//   - stretch the work window so the cap is observable without
//     waiting for the real ~50ms argon2 cost.
//
// Production never calls this. The hook is set on a single *Verifier
// instance, not globally — concurrent tests using different verifiers
// don't see each other's hooks.
func SetArgon2HookForTest(v *Verifier, hook func()) {
	v.argon2Hook = hook
}
