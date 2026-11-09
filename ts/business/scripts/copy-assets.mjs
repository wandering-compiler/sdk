// Copies what tsc does not emit into dist/: the SDK's own descriptor set.
import { copyFileSync, mkdirSync } from "node:fs";

const root = new URL("..", import.meta.url);
mkdirSync(new URL("dist/", root), { recursive: true });
copyFileSync(new URL("src/sdk_descriptors.binpb", root), new URL("dist/sdk_descriptors.binpb", root));
