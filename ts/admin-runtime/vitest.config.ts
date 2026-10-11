import { defineConfig } from "vitest/config";

// Test config for the hand-written admin-runtime logic tier.
//
// `jsdom` gives auth.ts a real `window.localStorage`. Coverage mirrors
// the Go coverage gate: v8 provider, thresholds gate the suite
// (FLOOR 70 / TARGET 90).
//
// SCOPE: coverage.include is the pure-logic surface plus the components
// that now HAVE render tests (@testing-library/react, `.test.tsx`).
// Components are a render seam, not a unit-depth target — the tests
// assert the decisions the component makes (does the Add button appear?
// does the create form POST the declared fields?), not its markup. Add
// a component's path here once its BEHAVIOUR is covered — not merely
// once it has a render test. ListPage/App have targeted tests (the Add
// button gate, the create route guard) but are not covered end to end,
// so gating them at the threshold would be a false signal.
//
// WHERE EACH COMPONENT ACTUALLY STANDS, re-measured 2026-09-29. The 2026-09-28
// figures this block used to carry were wrong in four rows, and the cause was the
// instrument: `src/vocab.test.ts` read the sources as `?raw`, which is a MODULE whose
// coverage entry collides with the real one on the same path. See that file's comment.
//
//                        stmts        branch       funcs
//   bootstrap.tsx        32/32         1/1          1/1     <- 0/32 until 2026-09-29; IN the gate now
//   Login.tsx           108/108       15/15         4/4     <- 97/108 + 12/15; IN the gate now
//   App.tsx              165/318       56/69         4/21
//   icons.tsx             68/96         7/7          7/12
//   OverviewPage.tsx     201/249       45/60        12/15
//   DetailPage.tsx       372/470       76/113       11/14
//   ListPage.tsx         459/583      155/203       20/39
//   ActionModal.tsx     112/112       15/15         6/6     <- 65/112, 1 of 5 funcs; IN the gate now
//   InlineSection.tsx    NOT in the gate, but its twin of ActionModal's crash is fixed
//                        and covered — see eventhandlers.test.ts
//   InlineSection.tsx    143/309       40/59         6/13    <- was recorded 2/100/0
//
// Counts, not percentages, because the arithmetic below has to be re-derivable: the
// gate's own base is 807/827 statements, 445/481 branch arms, 85/85 functions.
//
// ⚠️ `bootstrap.tsx` was quoted here as the one component meeting the rule, at
// 100/100/100, and it was 0 of 32 statements — nothing executed the SPA entry point at
// all, only the things it mounts. Twice now, from two unrelated causes, a component
// "reporting 100%" in this suite has meant the instrument was not reading it. That is
// why the counts are here and the percentages are derived from them.
//
// ✅ Three meet the rule for real now and are IN `coverage.include`:
//   bootstrap.tsx   — nothing had executed the SPA entry point (bootstrap.test.tsx)
//   Login.tsx       — the entire sign-in FAILURE path was untested (loginfailure.test.tsx)
//   ActionModal.tsx — nothing had ever SUBMITTED the modal; writing the first test that
//                     typed into an extra field found a live crash (actionmodal.test.tsx)
// The other six still do not meet it.
//
// ⚠️ The arithmetic below is HISTORICAL as of `perFile: true` — it is the route that
// used to exist, kept because it is the reason the route had to be closed. SIX could
// each be added ALONE without breaching the thresholds, because a 97.58% baseline
// absorbs one file's gaps: bootstrap, Login, icons, ActionModal, OverviewPage,
// DetailPage. None of that decides an admission any more: a per-file threshold asks
// only whether THAT file meets it.
//
//   bootstrap + Login + icons                        91.44 / 92.26 / 95.10   holds
//   Login + icons + OverviewPage                     91.64 / 90.41 / 93.10   holds
//   bootstrap + Login + OverviewPage + ActionModal   88.13                   breaches
//   all ten .tsx                                     76.58 / 82.60 / 72.25   breaches
//
// The largest set that holds is THREE (five such triples), not four — and the
// four-file set this block used to quote at "90.40, holds" is 88.13. A conclusion four
// tenths of a point from its threshold was reading an artefact.
//
// So nothing is added, and admitting ActionModal at 58% or OverviewPage at 80% because
// the aggregate can carry them is exactly the false signal this block warns about.
//
// ✅ The other half of that — "an AGGREGATE threshold over a dozen files does not
// protect any single file" — is FIXED rather than merely noted: `perFile: true` below.
// This block used to price it at "gating the eight components the rule says are not
// ready", and that price was imaginary: thresholds only see `coverage.include`, so the
// eight are not in the coverage map to gate. Measured — enabling it broke on one file
// and four tests closed it.
//
// The exclusion is right for the nine left out — together they carry 542 branch arms
// and 124 functions against 445/481 and 85/85 in the gate today, and admitting them
// fails all three thresholds at once (76.58 / 82.60 / 72.25).
export default defineConfig({
  test: {
    environment: "jsdom",
    setupFiles: ["./vitest.setup.ts"],

    // ONE worker, so the numbers below mean the same thing twice.
    //
    // v8 counts modules a worker LOADED, and the default pool gives each worker a
    // different subset — so this suite reported a different denominator every run:
    // 649/668, 667/686, 649/668 statements over an identical tree, a swing of 18
    // (2.7%). Every delta smaller than that was noise, which is a poor property for
    // a gate.
    //
    // One worker was unavailable until 2026-09-28 because four OverviewPage tests
    // failed under it about 37% of the time. The cause was a global: two test files
    // pinned navigator.language to "cs" and never put it back, and navigator is ONE
    // object per worker — so those tiles formatted 399 400 (narrow no-break space,
    // comma decimal) while the tests assert 399,400. Per-file workers had been
    // hiding it. Both files clear the override now.
    pool: "threads",
    poolOptions: { threads: { singleThread: true } },
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    coverage: {
      provider: "v8",
      include: [
        "src/api.ts",
        "src/auth.ts",
        "src/types.ts",
        "src/valueFormat.ts",
        "src/slotFormat.ts",
        "src/wireNumber.ts",
        "src/i18n.ts",
        "src/wire.ts",
        "src/cellFormat.ts",
        "src/CreatePage.tsx",
        "src/bootstrap.tsx",
        "src/Login.tsx",
        "src/ActionModal.tsx",
      ],
      reporter: ["text", "text-summary"],
      // perFile: EVERY included file has to meet the thresholds on its own, not just
      // the aggregate. The block above used to say this would cost "gating the eight
      // components the rule says are not ready" — that is wrong, and measuring it is
      // what made this change small: thresholds only see files in `coverage.include`,
      // and the eight excluded components are not in the coverage map at all. Turning
      // it on failed on exactly ONE thing, `cellFormat.ts` branches at 57.14%, whose
      // three uncovered arms were `adminFormatLocale`'s fallback chain — unreachable
      // from a suite that runs in jsdom, where `navigator` always exists and always
      // names a language. Three tests in cellformat.test.ts reach them, and exactly ONE
      // of the three is load-bearing for the count — the file says which and why the
      // other two stay anyway.
      //
      // What this buys, twice over:
      //
      //   - the aggregate could absorb one file going dark, and did — CreatePage.tsx sat
      //     at "100%" for a day with its 100 statements uncounted. A per-file
      //     threshold cannot be carried by its neighbours;
      //   - the admission rule at the top of this block stops being advice. "Add a
      //     component once its BEHAVIOUR is covered" was enforced by nothing: the
      //     arithmetic above is exactly how far a component BELOW the threshold could be
      //     admitted and carried. Now it has to meet the threshold itself.
      thresholds: { lines: 90, functions: 90, statements: 90, branches: 85, perFile: true },
    },
  },
});
