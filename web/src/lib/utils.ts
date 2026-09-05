import i18n from './i18n'

export function errorStatus(error:unknown){const status=(error as{status?:unknown})?.status;return typeof status==='number'?status:0}

export function errorText(error: unknown) {
  const message = error instanceof Error ? error.message : String(error)
  if (/failed to fetch|networkerror|load failed/i.test(message)) {
    return i18n.t('errors.apiUnavailable')
  }
  if (message.includes('model provider request failed')) {
    return i18n.t('errors.providerUnavailable',{message})
  }
  return message
}

export function formatFileSize(size:number){if(size<1024)return `${size} B`;if(size<1024**2)return `${(size/1024).toFixed(1)} KiB`;if(size<1024**3)return `${(size/1024**2).toFixed(1)} MiB`;return `${(size/1024**3).toFixed(1)} GiB`}

export function sshTunnelRoute(host:string,direction:string,localHost:string,localPort:number,remoteHost:string,remotePort:number,automatic='auto'){
	const local=`${localHost||'127.0.0.1'}:${localPort||automatic}`
	const remote=`${host}:${remoteHost||'127.0.0.1'}:${remotePort||automatic}`
	return direction==='reverse'?`${local} → ${remote}`:`${local} ← ${remote}`
}

export const desktopRuntime='__TAURI_INTERNALS__' in window

export function compactTokenCount(value:number){
	if(value<1000)return String(value)
	if(value<1_000_000)return `${Number((value/1000).toFixed(value<10_000?1:0))}K`
	return `${Number((value/1_000_000).toFixed(value<10_000_000?1:0))}M`
}

let clientIdCounter = 0
export function clientId() {
  try {
    if (typeof globalThis.crypto?.randomUUID === 'function') return globalThis.crypto.randomUUID()
    const random = new Uint32Array(2)
    globalThis.crypto?.getRandomValues(random)
    if (random[0] || random[1]) return `client_${random[0].toString(36)}${random[1].toString(36)}`
  } catch { /* insecure or legacy browser: rendering keys do not require cryptographic randomness */ }
  clientIdCounter += 1
  return `client_${Date.now().toString(36)}_${clientIdCounter.toString(36)}_${Math.random().toString(36).slice(2)}`
}
