export function appDestination(value, hostname) {
  const host = hostname.includes(':') && !hostname.startsWith('[') ? '[' + hostname + ']' : hostname;
  return value.replaceAll('{host}', host);
}
