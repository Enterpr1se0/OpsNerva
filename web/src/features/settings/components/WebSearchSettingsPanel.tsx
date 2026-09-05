import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { LoaderCircle, Save, Search } from 'lucide-react'
import { api } from '../../../api/api'
import { AppSelect } from '../../../components/Controls'
import { PasswordInput } from '../../../components/PasswordInput'
import { SettingsDisclosure } from '../../../components/SettingsDisclosure'
import { useNotifier } from '../../../lib/notifications'
import { errorText } from '../../../lib/utils'
import type { Proxy, WebSearchSettings, WebSearchSettingsInput } from '../../../types'

const defaultWebSearchInput:WebSearchSettingsInput={enabled:false,base_url:'https://api.tavily.com',api_key:'',proxy_id:'',timeout_seconds:20,max_results:10}

export function WebSearchSettingsPanel({proxies}:{proxies:Proxy[]}){
	const {t}=useTranslation()
	const notify=useNotifier()
	const [stored,setStored]=useState<WebSearchSettings|null>(null),[input,setInput]=useState<WebSearchSettingsInput>(defaultWebSearchInput)
	const [loading,setLoading]=useState(true),[busy,setBusy]=useState(''),[dirty,setDirty]=useState(false)
	const hasEffectiveAPIKey=!!input.api_key?.trim()||!!stored?.has_api_key&&!input.clear_api_key
	const applyStored=(value:WebSearchSettings)=>{setStored(value);setInput({enabled:value.enabled,base_url:value.base_url,api_key:'',proxy_id:value.proxy_id||'',timeout_seconds:value.timeout_seconds,max_results:value.max_results});setDirty(false)}
	useEffect(()=>{let active=true;api.webSearchSettings().then(value=>{if(active)applyStored(value)}).catch(err=>{if(active)notify(errorText(err),'error')}).finally(()=>{if(active)setLoading(false)});return()=>{active=false}},[notify])
	const update=<K extends keyof WebSearchSettingsInput>(key:K,value:WebSearchSettingsInput[K])=>{setInput(current=>({...current,[key]:value}));setDirty(true)}
	const save=async()=>{setBusy('save');try{const value=await api.saveWebSearchSettings(input);applyStored(value);notify(t('webSearch.saved'))}catch(err){notify(errorText(err),'error')}finally{setBusy('')}}
	const test=async()=>{setBusy('test');try{const result=await api.testWebSearch();notify(t('webSearch.testPassed',{count:result.results.length,latency:`${(result.response_time||0).toFixed(2)}s`,id:result.request_id||'—'}))}catch(err){notify(errorText(err),'error')}finally{setBusy('')}}
	const clearKey=()=>{setInput(current=>({...current,enabled:false,api_key:'',clear_api_key:true}));setDirty(true)}
	if(loading)return <SettingsDisclosure className="web-search-settings" icon={<Search size={18}/>} title={t('webSearch.title')} meta={t('common.loading')}><div className="settings-loading"><LoaderCircle className="spin" size={16}/>{t('common.loading')}</div></SettingsDisclosure>
	return <SettingsDisclosure className="web-search-settings" icon={<Search size={18}/>} title={t('webSearch.title')} meta={input.enabled?t('common.enabled'):t('common.disabled')}><label className="web-search-toggle"><span>{t('webSearch.title')}</span><input type="checkbox" checked={input.enabled} onChange={event=>update('enabled',event.target.checked)}/><i/><b>{input.enabled?t('common.enabled'):t('common.disabled')}</b></label><div className="web-search-grid"><label><span>{t('webSearch.baseURL')}</span><input value={input.base_url} onChange={event=>update('base_url',event.target.value)} placeholder="https://api.tavily.com"/></label><label><span>{t('webSearch.apiKey')}</span><PasswordInput value={input.api_key||''} onChange={event=>update('api_key',event.target.value)} placeholder={stored?.has_api_key?t('webSearch.savedSecret'):''}/></label><label><span>{t('common.proxy')}</span><AppSelect value={input.proxy_id||''} ariaLabel={t('common.proxy')} onChange={value=>update('proxy_id',value)} options={[{value:'',label:t('common.direct')},...proxies.map(proxy=>({value:proxy.id,label:`${proxy.name} · ${proxy.url}`}))]}/></label><label><span>{t('webSearch.timeout')}</span><input type="number" min="5" max="120" value={input.timeout_seconds} onChange={event=>update('timeout_seconds',Number(event.target.value))}/></label><label><span>{t('webSearch.maxResults')}</span><input type="number" min="1" max="20" value={input.max_results} onChange={event=>update('max_results',Number(event.target.value))}/></label></div><footer><div>{stored?.has_api_key&&<button type="button" className="danger" onClick={clearKey}>{t('webSearch.clearKey')}</button>}</div><button type="button" disabled={busy!==''||dirty||!stored?.enabled||!stored.has_api_key} onClick={()=>void test()}>{busy==='test'?<LoaderCircle className="spin" size={13}/>:<Search size={13}/>} {t('common.test')}</button><button type="button" className="primary" disabled={busy!==''||!dirty||input.enabled&&!hasEffectiveAPIKey} onClick={()=>void save()}>{busy==='save'?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {t('common.save')}</button></footer></SettingsDisclosure>
}
