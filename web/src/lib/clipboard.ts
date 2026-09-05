export async function writeClipboard(value:string){
	try{
		if(navigator.clipboard?.writeText){await navigator.clipboard.writeText(value);return}
	}catch{/* use the document fallback below */}
	const textarea=document.createElement('textarea')
	textarea.value=value;textarea.readOnly=true;textarea.style.position='fixed';textarea.style.opacity='0'
	document.body.appendChild(textarea);textarea.select()
	const copied=document.execCommand('copy')
	textarea.remove()
	if(!copied)throw new Error('clipboard write failed')
}
