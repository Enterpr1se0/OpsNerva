import { FormEvent, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import type { Terminal as XTermInstance } from '@xterm/xterm'
import { LoaderCircle, LockKeyhole, Power, RotateCw, Send, ShieldAlert, Square, TerminalSquare, X } from 'lucide-react'
import type { SSHShell, SSHShellEvent } from '../../../types'
import { api } from '../../../api/api'
import { AppSelect } from '../../../components/Controls'
import { PasswordInput } from '../../../components/PasswordInput'
import { writeClipboard } from '../../../lib/clipboard'
import { errorText } from '../../../lib/utils'
import { sshShellActive } from '../utils'
import { reconnectOperatorShell, sshShellCanReconnect } from '../shellState'
import { openShellStream, type ShellStream, type ShellStreamState } from '../shellStream'
import { SSHHostStatusBar } from './SSHHostStatusBar'

export function SSHShellTerminal({initialShell,relatedShells=[],onSelect,onClose,onChanged,onReconnected,onError,embedded=false}:{initialShell:SSHShell;relatedShells?:SSHShell[];onSelect?:(shell:SSHShell)=>void;onClose:()=>void;onChanged:()=>void;onReconnected:(previousID:string,shell:SSHShell)=>void;onError:(message:string)=>void;embedded?:boolean}){
	const {t}=useTranslation()
	const [shell,setShell]=useState(initialShell)
	const [previousInitial,setPreviousInitial]=useState(initialShell)
	if(previousInitial!==initialShell){setPreviousInitial(initialShell);setShell(initialShell)}
	const [streamState,setStreamState]=useState<ShellStreamState>('connecting')
	const [reconnecting,setReconnecting]=useState(false)
	const reconnectingRef=useRef(false)
	const [secret,setSecret]=useState('')
	const [sendingSecret,setSendingSecret]=useState(false)
	const [closing,setClosing]=useState(false)
	const terminalElement=useRef<HTMLDivElement>(null)
	const terminalRef=useRef<XTermInstance|null>(null)
	const streamRef=useRef<ShellStream|null>(null)
	const onChangedRef=useRef(onChanged)
	const onErrorRef=useRef(onError)
	useLayoutEffect(()=>{onChangedRef.current=onChanged;onErrorRef.current=onError},[onChanged,onError])
	const active=sshShellActive(shell.status)
	const connected=active&&streamState==='connected'

	useEffect(()=>{
		const container=terminalElement.current
		if(!container)return
		let disposed=false
		let cleanup=()=>{}
		void Promise.all([import('@xterm/xterm'),import('@xterm/addon-fit')]).then(([xtermModule,fitModule])=>{
			if(disposed)return
			const terminal=new xtermModule.Terminal({
				cursorBlink:false,
				disableStdin:true,
				convertEol:false,
				fontFamily:"'JetBrains Mono','Cascadia Code','SFMono-Regular',Consolas,monospace",
				fontSize:13,
				theme:{background:'#071019',foreground:'#d8e3ea',cursor:'#55d6be',selectionBackground:'#31546a'},
				scrollback:10_000,
			})
			const fit=new fitModule.FitAddon()
			terminal.loadAddon(fit)
			terminal.open(container)
			terminalRef.current=terminal
			terminal.attachCustomKeyEventHandler(event=>{
				if(event.type!=='keydown'||event.key.toLowerCase()!=='c'||!terminal.hasSelection())return true
				const copyShortcut=(event.metaKey||event.ctrlKey)&&!event.altKey
				if(!copyShortcut)return true
				event.preventDefault()
				void writeClipboard(terminal.getSelection()).catch(err=>onErrorRef.current(errorText(err))).finally(()=>terminal.focus())
				return false
			})
			let inputBuffer=''
			let inputTimer:number|undefined
			let outputFrame:number|undefined
			let outputBytes=0
			let outputChunks:Uint8Array[]=[]
			const outputEncoder=new TextEncoder()
			const flushOutput=()=>{
				outputFrame=undefined
				if(!outputBytes)return
				const combined=new Uint8Array(outputBytes)
				let offset=0
				for(const chunk of outputChunks){combined.set(chunk,offset);offset+=chunk.byteLength}
				outputChunks=[];outputBytes=0
				terminal.write(combined)
			}
			const queueOutput=(content:string|Uint8Array)=>{
				const chunk=typeof content==='string'?outputEncoder.encode(content):content
				if(!chunk.byteLength)return
				outputChunks.push(chunk);outputBytes+=chunk.byteLength
				if(outputFrame===undefined)outputFrame=requestAnimationFrame(flushOutput)
			}
			const sendCommand=(command:Record<string,unknown>)=>streamRef.current?.send(command)||false
			const flushInput=()=>{
				if(inputTimer!==undefined)window.clearTimeout(inputTimer)
				inputTimer=undefined
				if(!inputBuffer)return
				const input=inputBuffer.slice(0,64<<10)
				if(!sendCommand({type:'input',content:input})){inputBuffer='';return}
				inputBuffer=inputBuffer.slice(input.length)
				if(inputBuffer)inputTimer=window.setTimeout(flushInput,0)
			}
			const inputDisposable=terminal.onData(data=>{
				inputBuffer+=data
				if(data.includes('\r')||data.includes('\n')||data.includes('\x03'))flushInput()
				else if(inputTimer===undefined)inputTimer=window.setTimeout(flushInput,24)
			})
			let resizeTimer:number|undefined
			const resizeDisposable=terminal.onResize(({cols,rows})=>{
				if(resizeTimer!==undefined)window.clearTimeout(resizeTimer)
				resizeTimer=window.setTimeout(()=>{sendCommand({type:'resize',cols,rows})},80)
			})
			const observer=new ResizeObserver(()=>fit.fit())
			observer.observe(container)
			fit.fit()
			terminal.focus()

			const applyEvent=(event:SSHShellEvent)=>{
				if(event.content&&(event.stream==='stdout'||event.stream==='stderr'))queueOutput(event.content)
				if(event.status&&event.stream==='status'){
					setShell(current=>({...current,status:event.status as SSHShell['status'],last_sequence:event.sequence}))
					if(!sshShellActive(event.status)){
						void api.sshShell(initialShell.id,event.sequence).then(snapshot=>setShell(snapshot.shell)).catch(()=>{/* final state is already visible */})
						onChangedRef.current()
					}
				}
			}
			const stream=openShellStream(initialShell.id,{
				onState:state=>{
					if(disposed)return
					setStreamState(state)
					terminal.options.disableStdin=state!=='connected'
					terminal.options.cursorBlink=state==='connected'
					if(state==='connected')sendCommand({type:'resize',cols:terminal.cols,rows:terminal.rows})
					else{inputBuffer='';if(inputTimer!==undefined)window.clearTimeout(inputTimer);inputTimer=undefined}
				},
				onOutput:queueOutput,
				onEvent:applyEvent,
				onShell:setShell,
				onError:error=>onErrorRef.current(error),
				onUnavailable:()=>{
					setShell(current=>sshShellActive(current.status)?{...current,status:'failed',termination_reason:'connection_lost'}:current)
					onChangedRef.current()
				},
			})
			streamRef.current=stream
			cleanup=()=>{
				if(outputFrame!==undefined)cancelAnimationFrame(outputFrame)
				flushOutput()
				stream.close()
				if(streamRef.current===stream)streamRef.current=null
				observer.disconnect()
				inputDisposable.dispose()
				resizeDisposable.dispose()
				if(inputTimer!==undefined)window.clearTimeout(inputTimer)
				if(resizeTimer!==undefined)window.clearTimeout(resizeTimer)
				terminal.dispose()
				terminalRef.current=null
			}
			if(disposed)cleanup()
		}).catch(err=>onErrorRef.current(errorText(err)))
		return()=>{disposed=true;cleanup()}
	},[initialShell.id])

	const sendSecret=async(event:FormEvent)=>{
		event.preventDefault()
		if(!secret||!connected||sendingSecret)return
		setSendingSecret(true)
		try{
			if(!streamRef.current?.send({type:'input',content:`${secret}\r`,sensitive:true}))throw new Error(t('sshShell.streamEnded'))
			setSecret('');terminalRef.current?.focus()
		}
		catch(err){onError(errorText(err))}
		finally{setSendingSecret(false)}
	}
	const interrupt=()=>{if(!streamRef.current?.send({type:'interrupt'}))onError(t('sshShell.streamEnded'));terminalRef.current?.focus()}
	const reconnect=async()=>{
		if(reconnectingRef.current)return
		if(active){streamRef.current?.retry();return}
		if(!sshShellCanReconnect(shell))return
		reconnectingRef.current=true;setReconnecting(true)
		try{
			const replacement=await reconnectOperatorShell(shell)
			onReconnected(shell.id,replacement)
		}catch(err){onError(errorText(err))}
		finally{reconnectingRef.current=false;setReconnecting(false)}
	}
	const stop=async()=>{setClosing(true);try{setShell(await api.closeSSHShell(shell.id));onChanged();onClose()}catch(err){onError(errorText(err))}finally{setClosing(false)}}
	const titleID=`ssh-shell-terminal-title-${shell.id}`
	const workspaceShell=shell.kind==='workspace'
	const managedShell=['agent','mcp','workspace_agent'].includes(shell.surface)
		const terminal=<section className={`ssh-shell-terminal-dialog ${embedded?'embedded':''} ${workspaceShell?'':'monitored'}`} role={embedded?undefined:'dialog'} aria-modal={embedded?undefined:true} aria-labelledby={embedded?undefined:titleID}>
			{!embedded&&<header>
				<div><TerminalSquare size={20}/><span>{workspaceShell&&<small>{t('workspace.terminal')}</small>}<h2 id={titleID}>{workspaceShell?shell.workspace_id:shell.host_name||shell.host_id}</h2></span></div>
				<div className="ssh-shell-terminal-state">{workspaceShell&&relatedShells.length>1&&<AppSelect className="terminal-session-select" value={shell.id} ariaLabel={t('workspace.switchTerminal')} onChange={value=>{const selected=relatedShells.find(item=>item.id===value);if(selected)onSelect?.(selected)}} options={relatedShells.map(item=>({value:item.id,label:`${t(item.surface==='workspace_agent'?'workspace.agent':'workspace.operator')} · ${item.cwd||'.'}`}))}/>}<em className={shell.status}>{t(`statusLabels.${shell.status}`,{defaultValue:shell.status})}</em><code>{shell.elevated?'root':shell.user}</code>{!embedded&&<button type="button" onClick={onClose} title={t('common.close')}><X size={16}/></button>}</div>
			</header>}
			<div className="ssh-shell-terminal-screen" ref={terminalElement}/>
			{!workspaceShell&&<SSHHostStatusBar shell={shell}/>}
			<footer>
				{managedShell&&<form onSubmit={sendSecret}><LockKeyhole size={14}/><PasswordInput value={secret} onChange={event=>setSecret(event.target.value)} disabled={!connected||sendingSecret} placeholder={t('sshShell.sensitivePlaceholder')} autoComplete="off"/><button className="primary" disabled={!secret||!connected||sendingSecret}>{sendingSecret?<LoaderCircle className="spin" size={13}/>:<Send size={13}/>} {t('sshShell.sendSensitive')}</button></form>}
				<div>{((active&&streamState!=='connected')||sshShellCanReconnect(shell))&&<button type="button" disabled={reconnecting||closing||active&&streamState==='connecting'} onClick={()=>void reconnect()}>{reconnecting?<LoaderCircle className="spin" size={13}/>:<RotateCw size={13}/>} {t(reconnecting||active&&streamState==='connecting'?'sshShell.starting':'sshShell.reconnect')}</button>}<button type="button" disabled={!connected} onClick={interrupt}><Square size={10}/>{t('sshShell.interrupt')}</button><button type="button" className="danger" disabled={!active||closing||reconnecting} onClick={()=>void stop()}>{closing?<LoaderCircle className="spin" size={13}/>:<Power size={13}/>} {t('sshShell.closeSession')}</button></div>
			</footer>
			{shell.error&&<p className="ssh-shell-terminal-error"><ShieldAlert size={13}/>{shell.error}</p>}
		</section>
	return embedded?terminal:<div className="ssh-shell-terminal-backdrop">{terminal}</div>
}
