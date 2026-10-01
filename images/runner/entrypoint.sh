#!/bin/sh
# Keep Claude Code's temp directory (scratchpad, task state) on the persistent
# home. The CLI refuses a symlinked /tmp/claude-<uid>; CLAUDE_CODE_TMPDIR is
# the supported override.
set -eu
export CLAUDE_CODE_TMPDIR="${CLAUDE_CODE_TMPDIR:-${HOME}/.cache/claude-tmp}"
mkdir -p -m 0700 "${CLAUDE_CODE_TMPDIR}"

# Secret injection (spec section 9): the chart mounts the egress CA when Cilium
# intercepts TLS to an injected host. Tools must trust it or those hosts fail
# with a certificate error. SSL_CERT_FILE and friends REPLACE the trust store,
# so they get a combined bundle; NODE_EXTRA_CA_CERTS extends it, so it gets the
# CA alone.
if [ -n "${SHOCK_EGRESS_CA_FILE:-}" ] && [ -r "${SHOCK_EGRESS_CA_FILE}" ]; then
    bundle_dir="${HOME}/.cache/shock"
    bundle="${bundle_dir}/ca-bundle.crt"
    mkdir -p "${bundle_dir}"
    system_store=/etc/ssl/certs/ca-certificates.crt
    if [ -r "${system_store}" ]; then
        cat "${system_store}" "${SHOCK_EGRESS_CA_FILE}" > "${bundle}"
    else
        cat "${SHOCK_EGRESS_CA_FILE}" > "${bundle}"
    fi
    export SSL_CERT_FILE="${bundle}"          # OpenSSL: curl, git, .NET, uv, Go
    export CURL_CA_BUNDLE="${bundle}"
    export REQUESTS_CA_BUNDLE="${bundle}"     # python requests
    export PIP_CERT="${bundle}"
    export GIT_SSL_CAINFO="${bundle}"
    export NODE_EXTRA_CA_CERTS="${SHOCK_EGRESS_CA_FILE}"   # Node, npm, claude

    # A JDK reads its own keystore, not the variables above. Build one from the
    # JDK's cacerts plus the CA when this image ships a JDK.
    if command -v keytool >/dev/null 2>&1 && [ -n "${JAVA_HOME:-}" ] && [ -r "${JAVA_HOME}/lib/security/cacerts" ]; then
        truststore="${bundle_dir}/truststore.p12"
        if [ ! -f "${truststore}" ]; then
            keytool -importkeystore -noprompt \
                -srckeystore "${JAVA_HOME}/lib/security/cacerts" -srcstorepass changeit \
                -destkeystore "${truststore}" -deststoretype PKCS12 -deststorepass changeit >/dev/null 2>&1 || true
            keytool -importcert -noprompt -alias shock-egress-ca \
                -file "${SHOCK_EGRESS_CA_FILE}" \
                -keystore "${truststore}" -storetype PKCS12 -storepass changeit >/dev/null 2>&1 || true
        fi
        if [ -f "${truststore}" ]; then
            export JAVA_TOOL_OPTIONS="${JAVA_TOOL_OPTIONS:-} -Djavax.net.ssl.trustStore=${truststore} -Djavax.net.ssl.trustStorePassword=changeit -Djavax.net.ssl.trustStoreType=PKCS12"
        fi
    fi
fi

exec claude "$@"
