# The conformance suite's TypeScript business tier, installed the way the
# platform's image installs the SDK: build ts/business from source, `npm pack`
# it, install the .tgz into /app with --omit=dev, and run the author's
# package (the fixed NoteService, plain .ts run by Node's type stripping)
# from /app/service. Build context: the repository root (`make conformance-ts`).
FROM node:24 AS sdk
COPY ts/business /src
WORKDIR /src
RUN npm ci --no-audit --no-fund && npm run build && mkdir /pkg && npm pack --pack-destination /pkg

FROM node:24-slim
COPY --from=sdk /pkg /pkg
RUN npm install --prefix /app --omit=dev --no-audit --no-fund /pkg/*.tgz
COPY ts/business/tests/conformance/testdata /app/testdata
COPY ts/business/tests/conformance/service /app/service
WORKDIR /app/service
ENTRYPOINT ["node", "/app/node_modules/@w17/business/dist/main.js", "serve", "--manifest", "/app/service/w17-business.json", "--project-root", "/app/service"]
