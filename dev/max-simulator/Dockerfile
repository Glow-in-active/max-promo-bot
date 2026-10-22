# syntax=docker/dockerfile:1.7
FROM node:24-alpine@sha256:feb76650b7195319268c59cb2d419c732d6c499b13bf5296f16ab59eb1f0a4f3 AS upstream
WORKDIR /upstream
ADD --checksum=sha256:864a9cc490a80bd4f47f15121ea68a4d06210603b4eb4df966b7e70843a127e8 https://codeload.github.com/620x/telegram-max-bridge/tar.gz/2b4b2df7f16d2015077f29c23c7515894d2bf01f /upstream.tar.gz
RUN tar -xzf /upstream.tar.gz --strip-components=2 telegram-max-bridge-2b4b2df7f16d2015077f29c23c7515894d2bf01f/max-emulator \
    && tar -xOf /upstream.tar.gz telegram-max-bridge-2b4b2df7f16d2015077f29c23c7515894d2bf01f/LICENSE > LICENSE \
    && rm /upstream.tar.gz
RUN npm ci --ignore-scripts --no-audit --no-fund \
    && ./node_modules/.bin/tsc --noEmit false --outDir dist --rootDir src --target ES2022 --module NodeNext --moduleResolution NodeNext --esModuleInterop --skipLibCheck --strict --noUncheckedIndexedAccess src/*.ts \
    && npm prune --omit=dev --ignore-scripts --no-audit --no-fund

FROM upstream AS certificates
RUN apk add --no-cache openssl \
    && mkdir /tls \
    && openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 3650 \
       -subj /CN=simulator -addext subjectAltName=DNS:simulator \
       -keyout /tls/key.pem -out /tls/cert.pem

FROM node:24-alpine@sha256:feb76650b7195319268c59cb2d419c732d6c499b13bf5296f16ab59eb1f0a4f3 AS base
WORKDIR /simulator
COPY --from=upstream /upstream/dist ./vendor
COPY --from=upstream /upstream/node_modules ./node_modules
COPY --from=upstream /upstream/LICENSE ./vendor/LICENSE
COPY dev/max-simulator/package.json ./package.json
COPY dev/max-simulator/src ./src
COPY dev/max-simulator/public ./public
RUN mkdir -m 0555 tls
COPY --from=certificates --chown=65532:65532 --chmod=0400 /tls/key.pem ./tls/key.pem
COPY --from=certificates --chmod=0444 /tls/cert.pem ./tls/cert.pem

FROM base AS test
COPY dev/max-simulator/test ./test
CMD ["node", "--test"]

FROM base AS runtime
USER 65532:65532
EXPOSE 4010 4011
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=6 \
    CMD ["node", "src/healthcheck.mjs"]
CMD ["node", "src/index.mjs"]
