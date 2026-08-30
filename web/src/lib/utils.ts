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
