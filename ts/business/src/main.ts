#!/usr/bin/env node
// The `w17-business` command: `node dist/main.js serve|health ...`. See cli.ts.

import { main } from "./cli.js";

const code = await main(process.argv.slice(2));
process.exitCode = code;
// After a drain everything the tier opened is closed and the process ends by
// itself. Should a library keep a handle open anyway, this (unref'd: it does
// not itself hold the process) ends it rather than leaving the container to
// be killed.
setTimeout(() => process.exit(code), 3000).unref();
