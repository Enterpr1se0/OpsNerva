export const fileListOverscan=6

export function fileListWindow(count:number,scrollTop:number,viewportHeight:number,rowHeight:number){
	const top=Math.max(0,Math.min(scrollTop,Math.max(0,count*rowHeight-viewportHeight)))
	return {
		start:Math.max(0,Math.floor(top/rowHeight)-fileListOverscan),
		end:Math.min(count,Math.ceil((top+viewportHeight)/rowHeight)+fileListOverscan),
	}
}

export function fileListIndexes(window:{start:number;end:number},focused:number|null,count:number){
	const indexes=Array.from({length:window.end-window.start},(_,offset)=>window.start+offset)
	// Keep a focused row mounted while the pointer scrolls away from it.
	if(focused!==null&&focused>=0&&focused<count&&(focused<window.start||focused>=window.end))indexes.push(focused)
	return indexes.sort((a,b)=>a-b)
}
