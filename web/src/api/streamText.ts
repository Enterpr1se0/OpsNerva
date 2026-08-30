const streamTextBlockChars=4<<10

export type StreamText={
	readonly blocks:readonly string[]
	readonly tail:string
	readonly length:number
}

export const emptyStreamText:StreamText={blocks:[],tail:'',length:0}

function safeBlockEnd(value:string,limit:number){
	let end=Math.min(limit,value.length)
	if(end>0&&end<value.length){
		const previous=value.charCodeAt(end-1)
		const next=value.charCodeAt(end)
		if(previous>=0xD800&&previous<=0xDBFF&&next>=0xDC00&&next<=0xDFFF)end--
	}
	return end
}

function splitBlocks(value:string){
	const blocks:string[]=[]
	let remaining=value
	while(remaining.length>streamTextBlockChars){
		const end=safeBlockEnd(remaining,streamTextBlockChars)
		blocks.push(remaining.slice(0,end))
		remaining=remaining.slice(end)
	}
	return{blocks,tail:remaining}
}

export function streamTextFrom(value:string):StreamText{
	if(!value)return emptyStreamText
	const {blocks,tail}=splitBlocks(value)
	return{blocks,tail,length:value.length}
}

export function appendStreamText(current:StreamText|undefined,chunk:string):StreamText{
	const value=current||emptyStreamText
	if(!chunk)return value
	const combined=value.tail+chunk
	if(combined.length<=streamTextBlockChars)return{blocks:value.blocks,tail:combined,length:value.length+chunk.length}
	const split=splitBlocks(combined)
	return{blocks:[...value.blocks,...split.blocks],tail:split.tail,length:value.length+chunk.length}
}

export function streamTextValue(value:StreamText|undefined){
	if(!value)return''
	return value.blocks.length?value.blocks.join('')+value.tail:value.tail
}

export function streamTextTail(value:StreamText|undefined,limit:number){
	if(!value||limit<=0)return''
	if(value.tail.length>=limit)return value.tail.slice(-limit)
	let result=value.tail
	for(let index=value.blocks.length-1;index>=0&&result.length<limit;index--)result=value.blocks[index]+result
	return result.slice(-limit)
}
