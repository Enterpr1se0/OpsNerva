import type { FileTransferRecord, SFTPTextEncoding } from './types'

export const maxFilePreviewBytes=1<<20

export function isAbortError(error:unknown){return error instanceof DOMException&&error.name==='AbortError'}

export const sftpTransferKey=(hostID:string)=>`sftp:${hostID}`
export const workspaceTransferKey=(workspaceID:string)=>`workspace:${workspaceID}`
export const emptyFileTransferRecord:FileTransferRecord={active:null,conflict:null,uploadVersion:0}

export function remoteChildPath(parent:string,name:string){return parent==='/'?`/${name}`:`${parent}/${name}`}
export function remoteParentPath(value:string){if(!value||value==='/')return '/';const parts=value.split('/').filter(Boolean);parts.pop();return `/${parts.join('/')}`||'/'}
export function textFileName(name:string){
	const extension=name.toLowerCase().match(/(?:^|\.)([^./]+)$/)?.[1]||''
	return new Set(['txt','md','markdown','json','jsonl','yaml','yml','toml','ini','conf','config','env','properties','xml','html','htm','css','scss','less','js','jsx','ts','tsx','mjs','cjs','go','rs','py','rb','php','java','kt','kts','c','h','cc','cpp','hpp','cs','swift','sh','bash','zsh','fish','ps1','bat','cmd','sql','csv','tsv','log','service','socket','timer']).has(extension)
}
export function utf16Encoding(bytes:Uint8Array,name:string):SFTPTextEncoding|''{
	if(bytes.length>=2&&bytes[0]===0xff&&bytes[1]===0xfe)return'utf-16le'
	if(bytes.length>=2&&bytes[0]===0xfe&&bytes[1]===0xff)return'utf-16be'
	if(!textFileName(name)||bytes.length<4)return''
	const sample=Math.min(bytes.length,4096)
	let evenZeros=0,oddZeros=0,pairs=0
	for(let index=0;index+1<sample;index+=2){if(bytes[index]===0)evenZeros+=1;if(bytes[index+1]===0)oddZeros+=1;pairs+=1}
	if(!pairs)return''
	if(oddZeros/pairs>.3&&evenZeros/pairs<.1)return'utf-16le'
	if(evenZeros/pairs>.3&&oddZeros/pairs<.1)return'utf-16be'
	return''
}
export function decodeTextFile(buffer:ArrayBuffer,name:string){
	const bytes=new Uint8Array(buffer)
	const utf16=utf16Encoding(bytes,name)
	if(utf16)return{content:new TextDecoder(utf16,{fatal:true}).decode(bytes),binary:false,encoding:utf16}
	if(bytes.includes(0))return{content:'',binary:true,encoding:'utf-8' as const}
	try{return{content:new TextDecoder('utf-8',{fatal:true}).decode(bytes),binary:false,encoding:'utf-8' as const}}
	catch{
		if(textFileName(name)){
			try{return{content:new TextDecoder('gb18030',{fatal:true}).decode(bytes),binary:false,encoding:'gb18030' as const}}
			catch{/* invalid text remains binary */}
		}
		return{content:'',binary:true,encoding:'utf-8' as const}
	}
}
