# ghcr.io/wandering-compiler/sdk-admin-spa — the build image of a w17 admin SPA.
#
# Node with the admin SPA's dependency set installed at /workspace/node_modules:
# exactly go/tooling/adminspa/deps.json, the set the compiler writes into every
# admin bundle's spa/package.json, with its whole transitive tree locked at the
# time the SDK release was built (package-lock.json is kept beside it). The
# generated admin Dockerfile takes it `FROM --platform=$BUILDPLATFORM` as its
# spa stage, links the project's vendored @w17/admin-runtime into the SPA, and
# runs `vite build`: nothing is downloaded, and every build of one SDK release
# builds against the same tree.
#
# Built for each builder platform: vite's bundler ships native binaries per
# platform, picked at install time.
#
# The build context is this directory after the workflow wrote package.json
# from deps.json (jq); a hand build does the same:
#   jq '{name:"w17-admin-spa",private:true,dependencies,devDependencies}' \
#     go/tooling/adminspa/deps.json > images/admin-spa/package.json
ARG NODE_IMAGE=node:22-alpine
FROM ${NODE_IMAGE}
WORKDIR /workspace
COPY package.json ./
RUN npm install --no-audit --no-fund \
 && npm cache clean --force
LABEL org.opencontainers.image.source="https://github.com/wandering-compiler/sdk" \
      org.opencontainers.image.description="Node with the w17 admin SPA's dependencies installed, for the generated admin bundle's vite build" \
      org.opencontainers.image.licenses="MIT"
