"""Verified TLS connections with address fallback before any HTTP request is sent."""
import http.client
import base64
import socket
import ssl
from urllib import request as urlrequest
from urllib.parse import unquote, urlsplit


def _connect_direct_tls(host, port, timeout=6, context=None):
    context = context or ssl.create_default_context()
    addresses = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
    last_error = OSError("No addresses resolved for preview host")
    for family, kind, protocol, _, address in addresses:
        raw = socket.socket(family, kind, protocol)
        try:
            raw.settimeout(timeout)
            raw.connect(address)
            return context.wrap_socket(raw, server_hostname=host)
        except ssl.SSLCertVerificationError:
            raw.close()
            raise
        except OSError as error:
            raw.close()
            last_error = error
    raise last_error


class PreviewHTTPSConnection(http.client.HTTPSConnection):
    def __init__(self, host, port=None, timeout=6, context=None):
        super().__init__(host, port=port, timeout=timeout, context=context)
        proxy_url = urlrequest.getproxies().get("https")
        if proxy_url and not urlrequest.proxy_bypass(host):
            proxy = urlsplit(proxy_url)
            if proxy.scheme != "http" or not proxy.hostname:
                raise ValueError("Preview HTTPS probes require an http:// CONNECT proxy")
            headers = {}
            if proxy.username is not None:
                credentials = unquote(proxy.username) + ":" + unquote(proxy.password or "")
                headers["Proxy-Authorization"] = "Basic " + base64.b64encode(credentials.encode()).decode()
            self.set_tunnel(self.host, self.port, headers=headers)
            self.host, self.port = proxy.hostname, proxy.port or 80

    def connect(self):
        # Only connection establishment is retried. Tickets and mutations are sent once.
        if self._tunnel_host:
            # The standard library verifies the destination hostname after CONNECT.
            super().connect()
        else:
            self.sock = _connect_direct_tls(self.host, self.port, min(self.timeout, 6), self._context)
            self.sock.settimeout(self.timeout)


def connect_tls(host, port, timeout=6, context=None):
    connection = PreviewHTTPSConnection(host, port, timeout, context)
    try:
        connection.connect()
        sock, connection.sock = connection.sock, None
        return sock
    finally:
        connection.close()
