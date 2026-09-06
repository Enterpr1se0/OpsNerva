import { useTranslation } from 'react-i18next'
import { ExternalLink } from 'lucide-react'
import { numberValue, previewText, recordArray, textValue, type JsonRecord } from '../payload'

function publicWebLink(value:string){
	try{
		const parsed=new URL(value),host=parsed.hostname.toLowerCase().replace(/\.$/,'')
		if(!['http:','https:'].includes(parsed.protocol)||parsed.username||parsed.password||host==='localhost'||host.endsWith('.localhost')||host.endsWith('.local')||host.endsWith('.internal'))return
		const ipv4=host.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/)?.slice(1).map(Number)
		if(ipv4&&(ipv4.some(part=>part>255)||ipv4[0]===10||ipv4[0]===127||ipv4[0]===0||ipv4[0]===169&&ipv4[1]===254||ipv4[0]===172&&ipv4[1]>=16&&ipv4[1]<=31||ipv4[0]===192&&ipv4[1]===168))return
		if(host==='[::1]'||host==='::1'||/^\[?f[cd]/.test(host)||/^\[?fe[89ab]/.test(host))return
		return parsed
	}catch{return}
}

export function WebToolResult({tool,payload}:{tool:string;payload:JsonRecord}){
	const {t}=useTranslation()
	const results=recordArray(payload.results)
	const failures=recordArray(payload.failed_results)
	const responseTime=numberValue(payload.response_time)
	const credits=numberValue(payload.credits)
	const omitted=numberValue(payload.omitted_results)
	return <div className="web-tool-result">
		{results.length>0&&<div className="web-source-list">{results.map((result,index)=>{
			const parsed=publicWebLink(textValue(result.url))
			const content=textValue(result.content)||textValue(result.raw_content)
			const title=textValue(result.title)||parsed?.hostname||textValue(result.url)
			const truncated=result.truncated===true
			return <article className="web-source-card" key={`${textValue(result.url)}_${index}`}>
				<header><div><b>{title}</b><span>{parsed?.hostname||textValue(result.url)}</span></div>{parsed&&<a href={parsed.href} target="_blank" rel="noreferrer noopener" title={t('webSearch.openSource')} aria-label={t('webSearch.openSource')}><ExternalLink size={14}/></a>}</header>
				{content&&<p>{previewText(content,tool==='web_search'?2<<10:8<<10)}</p>}
				{(textValue(result.published_date)||numberValue(result.score)>0||truncated)&&<footer>{textValue(result.published_date)&&<time>{textValue(result.published_date)}</time>}{numberValue(result.score)>0&&<span>{Math.round(numberValue(result.score)*100)}%</span>}{truncated&&<em>{t('webSearch.truncated')}</em>}</footer>}
			</article>
		})}</div>}
		{(responseTime>0||credits>0||textValue(payload.request_id))&&<div className="web-tool-meta">{responseTime>0&&<span>{responseTime.toFixed(2)}s</span>}{credits>0&&<span>{t('webSearch.credits',{count:credits})}</span>}{textValue(payload.request_id)&&<code title={textValue(payload.request_id)}>{textValue(payload.request_id)}</code>}</div>}
		{failures.length>0&&<section className="web-source-failures"><b>{t('webSearch.failures')}</b>{failures.map((failure,index)=>{const parsed=publicWebLink(textValue(failure.url));return <div key={`${textValue(failure.url)}_${index}`}><span>{parsed?.hostname||textValue(failure.url)}</span><small>{textValue(failure.error)}</small></div>})}</section>}
		{omitted>0&&<div className="web-tool-omitted">{t('webSearch.omitted',{count:omitted})}</div>}
	</div>
}
