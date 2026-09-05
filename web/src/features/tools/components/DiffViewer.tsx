import { useTranslation } from 'react-i18next'
import { FileText } from 'lucide-react'
import { CopyButton } from '../../../components/CopyButton'
import { numberValue, previewText, textValue, toolDiffPreviewChars, type JsonRecord } from '../payload'

type DiffRow={kind:'header'|'hunk'|'add'|'delete'|'context'|'meta';oldLine?:number;newLine?:number;text:string}
function parseDiffRows(diff:string):DiffRow[]{
	let oldLine:number|undefined,newLine:number|undefined
	return diff.replace(/\n$/, '').split('\n').map(line=>{
		const hunk=line.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/)
		if(hunk){oldLine=Number(hunk[1]);newLine=Number(hunk[2]);return{kind:'hunk',text:line}}
		if(line.startsWith('@@ ')){oldLine=undefined;newLine=undefined;return{kind:'hunk',text:line}}
		if(line.startsWith('--- ')||line.startsWith('+++ '))return{kind:'header',text:line}
		if(line.startsWith('+')){const row={kind:'add' as const,newLine,text:line};if(newLine!==undefined)newLine++;return row}
		if(line.startsWith('-')){const row={kind:'delete' as const,oldLine,text:line};if(oldLine!==undefined)oldLine++;return row}
		if(line.startsWith(' ')){const row={kind:'context' as const,oldLine,newLine,text:line};if(oldLine!==undefined)oldLine++;if(newLine!==undefined)newLine++;return row}
		return{kind:'meta',text:line}
	})
}

export function DiffViewer({change}:{change:JsonRecord}){
	const {t}=useTranslation(),diff=textValue(change.diff),rows=parseDiffRows(previewText(diff,toolDiffPreviewChars))
	return <section className="diff-viewer"><header><span><FileText size={14}/>{t('tool.fileEdit')}</span><div><em className="add">+{numberValue(change.additions)}</em><em className="delete">-{numberValue(change.deletions)}</em><CopyButton value={diff}/></div></header><div className="diff-scroll" role="table" aria-label={t('tool.diff')}><div className="diff-lines">{rows.map((row,index)=><div className={`diff-line ${row.kind}`} role="row" key={index}><span className="old-line">{row.oldLine??''}</span><span className="new-line">{row.newLine??''}</span><code>{row.text||' '}</code></div>)}</div></div></section>
}
