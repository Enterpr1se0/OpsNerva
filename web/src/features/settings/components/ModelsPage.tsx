import { FormEvent, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Activity, Cpu, Edit3, Plus, RefreshCw, Trash2, Zap } from 'lucide-react'
import { api } from '../../../api/api'
import { AppSelect, ModelCombobox } from '../../../components/Controls'
import { localeFor } from '../../../lib/i18n'
import { PasswordInput } from '../../../components/PasswordInput'
import { useNotifier } from '../../../lib/notifications'
import { DestructiveConfirmDialog } from '../../../components/DestructiveConfirmDialog'
import { errorText, compactTokenCount } from '../../../lib/utils'
import type { ModelCatalog, ModelProvider, ModelProviderInput, ModelProviderKind, Proxy } from '../../../types'
import { Empty, FloatingPageActions } from '../../../components/PageLayout'
import { ConfigurationEditorPage, AddressVisibilityButton } from './ConfigurationLayout'
import { reasoningEfforts } from '../defaults'

const emptyProviderForm: ModelProviderInput = {name:'',kind:'openai',base_url:'',model:'gpt-4o-mini',context_window:null,reasoning_effort:'',api_key:'',proxy_id:'',user_agent:''}
const providerLabels: Record<ModelProviderKind,string> = {
  openai: 'OpenAI', deepseek: 'DeepSeek', anthropic: 'Anthropic', openai_compatible: 'OpenAI-compatible', ollama: 'Ollama',
}
const providerDefaults: Record<ModelProviderKind,Pick<ModelProviderInput,'base_url'|'model'>> = {
  openai: {base_url:'',model:'gpt-4o-mini'},
  deepseek: {base_url:'https://api.deepseek.com',model:'deepseek-v4-flash'},
  anthropic: {base_url:'https://api.anthropic.com',model:'claude-opus-4-8'},
  openai_compatible: {base_url:'',model:''},
  ollama: {base_url:'http://127.0.0.1:11434/v1',model:''},
}

export function ModelsPage({providers,proxies,showAddresses,onToggleAddresses,refresh}:{providers:ModelProvider[];proxies:Proxy[];showAddresses:boolean;onToggleAddresses:()=>void;refresh:()=>Promise<void>}) {
	const {t,i18n:instance}=useTranslation()
	const notify=useNotifier()
  const [showForm,setShowForm]=useState(false)
	  const [form,setForm]=useState<ModelProviderInput>(emptyProviderForm)
	  const [busy,setBusy]=useState('')
	  const [testing,setTesting]=useState<Set<string>>(()=>new Set())
		const [deleteCandidate,setDeleteCandidate]=useState<ModelProvider|null>(null)
  const [catalog,setCatalog]=useState<ModelCatalog|null>(null)
  const [discovering,setDiscovering]=useState(false)
	const [formErrors,setFormErrors]=useState<Partial<Record<'name'|'model'|'context_window',string>>>({})
  const editing=!!form.id

	const updateForm=<K extends keyof ModelProviderInput>(field:K,value:ModelProviderInput[K])=>{setForm(current=>({...current,[field]:value}));setFormErrors(current=>{if(!current[field as keyof typeof current])return current;const next={...current};delete next[field as keyof typeof next];return next})}
  const openCreate=()=>{setForm(emptyProviderForm);setCatalog(null);setFormErrors({});setShowForm(true)}
	const openEdit=(provider:ModelProvider)=>{setForm({id:provider.id,name:provider.name,kind:provider.kind,base_url:provider.base_url||'',model:provider.model,context_window:provider.context_window||null,reasoning_effort:provider.reasoning_effort||'',api_key:'',proxy_id:provider.proxy_id||'',user_agent:provider.user_agent||''});setCatalog(null);setFormErrors({});setShowForm(true)}
	const changeKind=(kind:ModelProviderKind)=>{setCatalog(null);setFormErrors({});setForm({...form,kind,context_window:null,...providerDefaults[kind]})}
	const discover=async()=>{setDiscovering(true);try{const {name:_name,model:_model,context_window:_contextWindow,reasoning_effort:_reasoningEffort,...payload}=form;const result=await api.discoverModels(payload);setCatalog(result);setForm(current=>({...current,model:result.models.includes(current.model)?current.model:''}));notify(t('models.found',{count:result.count}))}catch(err){setCatalog(null);notify(errorText(err),'error')}finally{setDiscovering(false)}}
	const selectedMetadata=catalog?.metadata?.[form.model]
		const setTestRunning=(key:string,running:boolean)=>setTesting(current=>{const next=new Set(current);if(running)next.add(key);else next.delete(key);return next})
		const testForm=async()=>{const key='form';setTestRunning(key,true);try{const {name:_name,context_window:_contextWindow,...payload}=form;const result=await api.testModelConfiguration(payload);notify(t('models.healthy',{name:result.model,response:result.response,latency:result.latency_ms}))}catch(err){notify(errorText(err),'error')}finally{setTestRunning(key,false)}}
	const validate=()=>{const errors:typeof formErrors={};if(!form.name.trim())errors.name=t('common.required');if(!form.model.trim())errors.model=t('common.required');if(form.context_window!==null&&(form.context_window<1024||form.context_window>10_000_000))errors.context_window=t('models.contextRange');setFormErrors(errors);return !Object.keys(errors).length}
	const save=async(event:FormEvent)=>{event.preventDefault();if(!validate())return;setBusy('save');try{const saved=await api.saveModelProvider({...form,name:form.name.trim(),model:form.model.trim(),context_window:form.context_window??0});notify(t('models.saved',{name:saved.name}));setShowForm(false);setForm(emptyProviderForm);setFormErrors({});await refresh()}catch(err){notify(errorText(err),'error')}finally{setBusy('')}}
	const activate=async(provider:ModelProvider)=>{setBusy(provider.id);try{await api.activateModelProvider(provider.id);notify(t('models.activated',{name:provider.name}));await refresh()}catch(err){notify(errorText(err),'error')}finally{setBusy('')}}
		const test=async(provider:ModelProvider)=>{setTestRunning(provider.id,true);try{const result=await api.testModelProvider(provider.id);notify(t('models.healthy',{name:provider.name,response:result.response,latency:result.latency_ms}))}catch(err){notify(errorText(err),'error')}finally{setTestRunning(provider.id,false)}}
	const remove=async()=>{const provider=deleteCandidate;if(!provider)return;setBusy(`delete-${provider.id}`);try{await api.deleteModelProvider(provider.id);notify(t('models.deleted',{name:provider.name}));await refresh()}catch(err){notify(errorText(err),'error')}finally{setBusy('');setDeleteCandidate(null)}}

  return <div className="page-stack has-floating-actions">
	{!showForm&&<FloatingPageActions><AddressVisibilityButton visible={showAddresses} onToggle={onToggleAddresses}/><button className="primary" onClick={openCreate}><Plus size={16}/>{t('models.add')}</button></FloatingPageActions>}
    {showForm&&<ConfigurationEditorPage icon={<Cpu size={22}/>} title={editing?t('models.editTitle'):t('models.newTitle')} busy={!!busy} onBack={()=>setShowForm(false)}><form className="model-form configuration-editor-form panel" noValidate onSubmit={save}>
      <div className="form-grid model-fields">
		<label className={formErrors.name?'invalid':''}><span>{t('models.displayName')}</span><input value={form.name} aria-invalid={!!formErrors.name} onChange={event=>updateForm('name',event.target.value)}/>{formErrors.name&&<small className="form-field-error">{formErrors.name}</small>}</label>
		<label><span>{t('models.providerType')}</span><AppSelect value={form.kind} ariaLabel={t('models.providerType')} onChange={value=>changeKind(value as ModelProviderKind)} options={(Object.keys(providerLabels) as ModelProviderKind[]).map(kind=>({value:kind,label:providerLabels[kind]}))}/></label>
		<label className={`model-id-field ${formErrors.model?'invalid':''}`}><span className="field-title"><span>{t('models.modelId')}</span><button type="button" onClick={discover} disabled={discovering}><RefreshCw size={12}/>{discovering?t('models.fetching'):t('models.fetchModels')}</button></span><ModelCombobox value={form.model} models={catalog?.models||[]} metadata={catalog?.metadata} onChange={value=>updateForm('model',value)} placeholder={t('models.modelPlaceholder')} ariaLabel={t('models.modelId')} invalid={!!formErrors.model}/>{formErrors.model&&<small className="form-field-error">{formErrors.model}</small>}</label>
			<label><span>{t('models.reasoningEffort')}</span><AppSelect value={form.reasoning_effort||'—'} ariaLabel={t('models.reasoningEffort')} onChange={value=>updateForm('reasoning_effort',value as ModelProviderInput['reasoning_effort'])} options={reasoningEfforts.map(value=>({value,label:value}))}/></label>
			<label className={formErrors.context_window?'invalid':''}><span>{t('models.contextWindow')}</span><input inputMode="numeric" value={form.context_window??''} aria-invalid={!!formErrors.context_window} onChange={event=>{const value=event.target.value.replace(/\D/g,'').slice(0,8);updateForm('context_window',value?Number(value):null)}} placeholder={selectedMetadata?.context_window?compactTokenCount(selectedMetadata.context_window):t('models.contextAuto')}/>{formErrors.context_window&&<small className="form-field-error">{formErrors.context_window}</small>}</label>
			<label><span>{t('models.apiKey')}</span><PasswordInput autoComplete="new-password" value={form.api_key} onChange={event=>{setCatalog(null);setForm({...form,api_key:event.target.value})}} placeholder={editing?t('models.keepKey'):''}/></label>
			<label className="base-url-field"><span>{t('models.baseUrl')}</span><input value={form.base_url} onChange={event=>{setCatalog(null);setForm({...form,base_url:event.target.value})}} placeholder={form.kind==='openai'?t('models.officialEndpoint'):''}/></label>
			<label><span>{t('models.userAgent')}</span><input value={form.user_agent} onChange={event=>{setCatalog(null);setForm({...form,user_agent:event.target.value})}} placeholder={t('models.userAgentHint')}/></label>
			<label className="proxy-select-field"><span>{t('common.proxy')}</span><AppSelect value={form.proxy_id} ariaLabel={t('common.proxy')} onChange={value=>{setCatalog(null);updateForm('proxy_id',value)}} options={[{value:'',label:t('common.direct')},...proxies.map(proxy=>({value:proxy.id,label:`${proxy.name} · ${proxy.url}`}))]}/></label>
      </div>
		  <div className="form-actions"><button type="button" onClick={()=>setShowForm(false)}>{t('common.cancel')}</button><button type="button" className="test-config" onClick={testForm} disabled={!!busy||testing.has('form')||!form.model}><Activity size={14}/>{testing.has('form')?t('models.sendingHello'):t('models.testModel')}</button><button className="primary" disabled={!!busy}>{busy==='save'?t('common.saving'):t('models.saveProvider')}</button></div>
    </form></ConfigurationEditorPage>}
    {!showForm&&<div className="model-grid">{providers.map(provider=>{const proxy=proxies.find(item=>item.id===provider.proxy_id);const contextWindow=provider.context_window||provider.resolved_context_window||0;return <article className={`model-card panel ${provider.active?'active':''}`} key={provider.id}>
	  <div className="model-card-head"><div className="provider-glyph"><Cpu size={21}/></div><div><h3>{provider.name}</h3><span>{providerLabels[provider.kind]}</span></div>{provider.active&&<em><Zap size={12}/>{t('models.active')}</em>}</div>
      <div className="model-name">{provider.model}</div>
	  <dl><div><dt>{t('models.endpoint')}</dt><dd>{provider.base_url?(showAddresses?provider.base_url:'••••••'):t('models.providerDefault')}</dd></div><div><dt>{t('models.contextWindow')}</dt><dd>{contextWindow?compactTokenCount(contextWindow):t('models.contextAuto')}</dd></div><div><dt>{t('models.proxy')}</dt><dd title={showAddresses?proxy?.url:undefined}>{proxy?.name||t('models.noProxy')}</dd></div>{provider.user_agent&&<div><dt>{t('models.userAgent')}</dt><dd>{provider.user_agent}</dd></div>}<div><dt>{t('models.credential')}</dt><dd>{provider.has_api_key?t('models.encryptedKey'):t('models.noApiKey')}</dd></div><div><dt>{t('common.updated')}</dt><dd>{new Date(provider.updated_at).toLocaleString(localeFor(instance.language))}</dd></div></dl>
		  <div className="model-actions"><button onClick={()=>test(provider)} disabled={!!busy||testing.has(provider.id)}><Activity size={14}/>{testing.has(provider.id)?t('common.testing'):t('common.test')}</button><button onClick={()=>openEdit(provider)} disabled={!!busy}><Edit3 size={14}/>{t('common.edit')}</button>{!provider.active&&<button className="use-model" onClick={()=>activate(provider)} disabled={!!busy}><Zap size={14}/>{busy===provider.id?t('models.switching'):t('models.useModel')}</button>}<button className="danger" title={t('common.delete')} onClick={()=>setDeleteCandidate(provider)} disabled={!!busy}><Trash2 size={14}/></button></div>
    </article>})}</div>}
		{!showForm&&!providers.length&&<Empty icon={<Cpu/>} title={t('models.emptyTitle')}/>}
		{deleteCandidate&&<DestructiveConfirmDialog
			title={t('models.deleteTitle',{name:deleteCandidate.name})}
			busy={busy===`delete-${deleteCandidate.id}`}
			onCancel={()=>setDeleteCandidate(null)}
			onConfirm={()=>void remove()}
		/>}
  </div>
}
