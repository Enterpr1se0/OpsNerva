export function sshShellActive(status:string){return ['starting','running','stopping'].includes(status)}

export function statusByteUnit(value:number){
	if(value>=1024**4)return{size:1024**4,label:'TiB'}
	if(value>=1024**3)return{size:1024**3,label:'GiB'}
	if(value>=1024**2)return{size:1024**2,label:'MiB'}
	if(value>=1024)return{size:1024,label:'KiB'}
	return{size:1,label:'B'}
}

export function formatStatusBytes(value:number){
	const unit=statusByteUnit(value)
	const scaled=value/unit.size
	return `${scaled>=100?scaled.toFixed(0):scaled>=10?scaled.toFixed(1):scaled.toFixed(2)} ${unit.label}`
}

export function formatStatusPair(used:number,total:number){
	const unit=statusByteUnit(total)
	const digits=total/unit.size>=100?0:1
	return `${(used/unit.size).toFixed(digits)} / ${(total/unit.size).toFixed(digits)} ${unit.label}`
}

export function formatHostUptime(seconds:number){
	const days=Math.floor(seconds/86400)
	const hours=Math.floor(seconds%86400/3600)
	const minutes=Math.floor(seconds%3600/60)
	return days>0?`${days}d ${hours}h`:hours>0?`${hours}h ${minutes}m`:`${minutes}m`
}